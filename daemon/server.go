package daemon

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/disorganizer/brig/daemon/proto"
	"github.com/disorganizer/brig/fuse"
	"github.com/disorganizer/brig/im"
	"github.com/disorganizer/brig/repo"
	"github.com/disorganizer/brig/util/tunnel"
	protobuf "github.com/gogo/protobuf/proto"
	"golang.org/x/net/context"
)

const (
	// MaxConnections is the upper limit of clients that may connect to a daemon
	// at the same time. Other client will wait in Accept().
	MaxConnections = 20
)

// This is just here to make the maxConnections prettier.
type allowOneConn struct{}

// Server is a TCP server that executed all commands
// on a single repository. Once the daemon is started, it
// attempts to open the repository, for which a password is needed.
type Server struct {
	// The repo we're working on
	Repo *repo.Repository

	// XMPP is the control client to the outside world.
	XMPP *im.Client

	// All mountpoints this daemon is serving:
	Mounts *fuse.MountTable

	// signals (external and self triggered) arrive on this channel.
	signals chan os.Signal

	// Root context for this daemon
	ctx context.Context

	// TCP Listener for incoming connections:
	listener net.Listener

	// buffered channel with N places,
	// every active connection holds one.
	maxConnections chan allowOneConn
}

// Summon creates a new up and running Server instance
func Summon(pwd, repoFolder string, port int) (*Server, error) {
	// Load the on-disk repository:
	log.Infof("Opening repo: %s", repoFolder)
	repository, err := repo.Open(pwd, repoFolder)
	if err != nil {
		log.Error("Could not load repository: ", err)
		return nil, err
	}

	// Listen for incoming connections.
	addr := fmt.Sprintf("localhost:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("Error listening:", err.Error())
		return nil, err
	}

	// TODO: Uncomment later
	// xmppClient, err := im.NewClient(
	// 	&im.Config{
	// 		Jid:      xmpp.JID(repository.Jid),
	// 		Password: pwd,
	// 		TLSConfig: tls.Config{
	// 			ServerName: xmpp.JID(repository.Jid).Domain(),
	// 		},
	// 		KeyPath:              filepath.Join(repository.InternalFolder, "otr.key"),
	// 		FingerprintStorePath: filepath.Join(repository.InternalFolder, "otr.buddies"),
	// 	},
	// )
	// if err != nil {
	// 	return nil, err
	// }

	// Close the listener when the application closes.
	log.Info("Listening on ", addr)

	ctx, cancel := context.WithCancel(context.Background())
	daemon := &Server{
		Repo:   repository,
		Mounts: fuse.NewMountTable(repository.Store),
		// XMPP:     xmppClient,
		signals:        make(chan os.Signal, 1),
		listener:       listener,
		maxConnections: make(chan allowOneConn, MaxConnections),
		ctx:            ctx,
	}

	go daemon.loop(cancel)
	return daemon, nil
}

// Serve waits until the Server received a quit reason.
func (d *Server) Serve() {
	<-d.ctx.Done()
	d.listener.Close()

	if err := d.Mounts.Close(); err != nil {
		log.Errorf("Error while closing mounts: %v", err)
	}

	if err := d.Repo.Close(); err != nil {
		log.Errorf("Unable to close repository: %v", err)
	}
}

// Handle incoming connections:
func (d *Server) loop(cancel context.CancelFunc) {
	// Forward signals to the signals channel:
	signal.Notify(d.signals, os.Interrupt, os.Kill)

	// Reserve at least cap(d.maxConnections)
	for i := 0; i < cap(d.maxConnections); i++ {
		d.maxConnections <- allowOneConn{}
	}

	for {
		select {
		case <-d.signals:
			// Break the Serve() loop
			cancel()
			return
		case <-d.maxConnections:
			// Listen for an incoming connection.
			deadline := time.Now().Add(500 * time.Millisecond)
			err := d.listener.(*net.TCPListener).SetDeadline(deadline)
			if err != nil {
				log.Errorf("BUG: SetDeadline failed: %v", err)
				return
			}

			conn, err := d.listener.Accept()
			if err != nil && err.(*net.OpError).Timeout() {
				d.maxConnections <- allowOneConn{}
				continue
			}

			if err != nil {
				log.Errorf("Error in Accept(): %v", err)
				return
			}

			// Handle connections in a new goroutine.
			go d.handleConnection(d.ctx, conn)
		default:
			log.Infof("Max number of connections hit: %d", cap(d.maxConnections))
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// Handles incoming requests:
func (d *Server) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Make sure this connection count gets released:
	defer func() {
		d.maxConnections <- allowOneConn{}
	}()

	tnl, err := tunnel.NewEllipticTunnel(conn)
	if err != nil {
		log.Error("Tunnel failed", err)
		return
	}

	// Loop until client disconnect or dies otherwise:
	for {
		msg := &proto.Command{}
		if err := recv(tnl, msg); err != nil {
			if err != io.EOF {
				log.Warning("daemon-recv: ", err)
			}
			return
		}

		log.Infof("recv: %s: %v", conn.RemoteAddr().String(), msg)
		d.handleCommand(ctx, msg, tnl)
	}
}

// Handles the actual incoming commands:
func (d *Server) handleCommand(ctx context.Context, cmd *proto.Command, conn io.ReadWriter) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Prepare a response template:
	resp := &proto.Response{
		ResponseType: cmd.CommandType,
		Success:      protobuf.Bool(false),
	}

	// Figure out which handler to call:
	handlerID := *(cmd.CommandType)
	if handler, ok := HandlerMap[handlerID]; !ok {
		resp.Error = protobuf.String(fmt.Sprintf("No handler for Id: %v", handlerID))
	} else {
		answer, err := handler(d, ctx, cmd)

		if err != nil {
			resp.Error = protobuf.String(err.Error())
		} else {
			resp.Response = protobuf.String(answer)
			resp.Success = protobuf.Bool(true)
		}
	}

	// Send the response back to the client:
	if err := send(conn, resp); err != nil {
		log.Warning("Unable to send message back to client: ", err)
	}
}
