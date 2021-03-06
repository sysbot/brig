package cmd

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/sahib/brig/cmd/tabwriter"

	"github.com/sahib/brig/client"
	"github.com/urfave/cli"
	yml "gopkg.in/yaml.v2"
)

func handleOffline(ctx *cli.Context, ctl *client.Client) error {
	return ctl.NetDisconnect()
}

func handleOnline(ctx *cli.Context, ctl *client.Client) error {
	return ctl.NetConnect()
}

func handleIsOnline(ctx *cli.Context, ctl *client.Client) error {
	self, err := ctl.Whoami()
	if err != nil {
		return err
	}

	if self.IsOnline {
		fmt.Println(color.GreenString("online"))
	} else {
		fmt.Println(color.RedString("offline"))
	}

	return nil
}

func handleRemoteList(ctx *cli.Context, ctl *client.Client) error {
	if ctx.Bool("offline") {
		return handleRemoteListOffline(ctx, ctl)
	}

	return handleRemoteListOnline(ctx, ctl)
}

func handleRemoteListOffline(ctx *cli.Context, ctl *client.Client) error {
	remotes, err := ctl.RemoteLs()
	if err != nil {
		return fmt.Errorf("remote ls: %v", err)
	}

	if ctx.IsSet("format") {
		tmpl, err := readFormatTemplate(ctx)
		if err != nil {
			return err
		}

		for _, remote := range remotes {
			if err := tmpl.Execute(os.Stdout, remote); err != nil {
				return err
			}
		}

		return nil
	}

	if len(remotes) == 0 {
		fmt.Println("No remotes yet. Use `brig remote add »user« »fingerprint«` to add some.")
		return nil
	}

	tabW := tabwriter.NewWriter(
		os.Stdout, 0, 0, 2, ' ',
		tabwriter.StripEscape,
	)

	fmt.Fprintln(tabW, "NAME\tFINGERPRINT\t")

	for _, remote := range remotes {
		fmt.Fprintf(tabW, "%s\t%s\t\n", remote.Name, remote.Fingerprint)
	}

	return tabW.Flush()
}

func handleRemoteListOnline(ctx *cli.Context, ctl *client.Client) error {
	peers, err := ctl.OnlinePeers()
	if err != nil {
		return err
	}

	tabW := tabwriter.NewWriter(
		os.Stdout, 0, 0, 2, ' ',
		tabwriter.StripEscape,
	)

	if len(peers) == 0 {
		fmt.Println("Remote list is empty. Nobody there to ping.")
		return nil
	}

	if !ctx.IsSet("format") {
		fmt.Fprintln(tabW, "NAME\tFINGERPRINT\tROUNDTRIP\tONLINE\tAUTHENTICATED\tLASTSEEN\t")
	}

	tmpl, err := readFormatTemplate(ctx)
	if err != nil {
		return err
	}

	for _, peer := range peers {
		if tmpl != nil {
			rmt := client.Remote{
				Fingerprint: peer.Fingerprint,
				Name:        peer.Name,
			}

			if err := tmpl.Execute(os.Stdout, rmt); err != nil {
				return err
			}

			continue
		}

		roundtrip := peer.Roundtrip.String()
		isOnline := color.GreenString("✔ ")

		if peer.Err != nil {
			isOnline = color.RedString("✘ " + peer.Err.Error())
			roundtrip = "∞"
		}

		authenticated := color.RedString("✘")
		if peer.Authenticated {
			authenticated = color.GreenString("✔")
		}

		shortFp := ""

		splitFp := strings.SplitN(peer.Fingerprint, ":", 2)
		if len(splitFp) > 0 {
			shortAddr := splitFp[0]
			if len(shortAddr) > 12 {
				shortAddr = shortAddr[:12]
			}

			shortFp += shortAddr
		}

		if len(splitFp) > 1 {
			shortPubKeyID := splitFp[1]
			if len(shortPubKeyID) > 12 {
				shortPubKeyID = shortPubKeyID[:12]
			}

			shortFp += ":"
			shortFp += shortPubKeyID
		}

		fmt.Fprintf(
			tabW,
			"%s\t%s\t%s\t%s\t%s\t%s\t\n",
			peer.Name,
			shortFp,
			roundtrip,
			isOnline,
			authenticated,
			peer.LastSeen.Format(time.UnixDate),
		)
	}

	return tabW.Flush()
}

const (
	remoteHelpText = `# No remotes yet. Uncomment the next lines for an example:
# - Name: alice@wonderland.com
#   Fingerprint: QmVA5j2JHPkDTHgZ[...]:SEfXUDeJA1toVnP[...]
`
)

func remoteListToYml(remotes []client.Remote) ([]byte, error) {
	if len(remotes) == 0 {
		// Provide a helpful description, instead of an empty list.
		return []byte(remoteHelpText), nil
	}

	return yml.Marshal(remotes)
}

func ymlToRemoteList(data []byte) ([]client.Remote, error) {
	remotes := []client.Remote{}

	if err := yml.Unmarshal(data, &remotes); err != nil {
		return nil, err
	}

	return remotes, nil
}

func handleRemoteAdd(ctx *cli.Context, ctl *client.Client) error {
	remote := client.Remote{
		Name:        ctx.Args().Get(0),
		Fingerprint: ctx.Args().Get(1),
		Folders:     nil,
	}

	if err := ctl.RemoteAdd(remote); err != nil {
		return fmt.Errorf("remote add: %v", err)
	}

	return nil
}

func handleRemoteRemove(ctx *cli.Context, ctl *client.Client) error {
	name := ctx.Args().First()
	if err := ctl.RemoteRm(name); err != nil {
		return fmt.Errorf("remote rm: %v", err)
	}

	return nil
}

func handleRemoteClear(ctx *cli.Context, ctl *client.Client) error {
	return ctl.RemoteClear()
}

func handleRemoteEdit(ctx *cli.Context, ctl *client.Client) error {
	remotes, err := ctl.RemoteLs()
	if err != nil {
		return fmt.Errorf("remote ls: %v", err)
	}

	data, err := remoteListToYml(remotes)
	if err != nil {
		return fmt.Errorf("Failed to convert to yml: %v", err)
	}

	// Launch an editor on the received data:
	newData, err := edit(data, "yml")
	if err != nil {
		return fmt.Errorf("Failed to launch editor: %v", err)
	}

	// Save a few network roundtrips if nothing was changed:
	if bytes.Equal(data, newData) {
		fmt.Println("Nothing changed.")
		return nil
	}

	newRemotes, err := ymlToRemoteList(newData)
	if err != nil {
		return err
	}

	if err := ctl.RemoteSave(newRemotes); err != nil {
		return fmt.Errorf("Saving back remotes failed: %v", err)
	}

	return nil
}

func findRemoteForName(ctl *client.Client, name string) (*client.Remote, error) {
	remotes, err := ctl.RemoteLs()
	if err != nil {
		return nil, err
	}

	for _, remote := range remotes {
		if remote.Name == name {
			return &remote, nil
		}
	}

	return nil, fmt.Errorf("No such remote with this name: %s", name)
}

func handleRemoteFolderAdd(ctx *cli.Context, ctl *client.Client) error {
	remote, err := findRemoteForName(ctl, ctx.Args().First())
	if err != nil {
		return err
	}

	for _, folder := range ctx.Args().Tail() {
		if _, err := ctl.Stat(folder); err != nil {
			fmt.Printf("warning: »%s« has no stat info: %s\n", folder, err)
		}

		remote.Folders = append(remote.Folders, folder)
	}

	return ctl.RemoteUpdate(*remote)
}

func handleRemoteFolderRemove(ctx *cli.Context, ctl *client.Client) error {
	remote, err := findRemoteForName(ctl, ctx.Args().First())
	if err != nil {
		return err
	}

	folderName := ctx.Args().Get(1)
	newFolders := []string{}
	for _, folder := range remote.Folders {
		if string(folder) == folderName {
			continue
		}

		newFolders = append(newFolders, folder)
	}

	remote.Folders = newFolders
	return ctl.RemoteUpdate(*remote)
}

func handleRemoteFolderClear(ctx *cli.Context, ctl *client.Client) error {
	remote, err := findRemoteForName(ctl, ctx.Args().First())
	if err != nil {
		return err
	}

	remote.Folders = []string{}
	return ctl.RemoteUpdate(*remote)
}

func handleRemoteFolderList(ctx *cli.Context, ctl *client.Client) error {
	remote, err := findRemoteForName(ctl, ctx.Args().First())
	if err != nil {
		return err
	}

	if len(remote.Folders) == 0 {
		fmt.Println("No folders specified. All folders are accessible.")
		return nil
	}

	for _, folder := range remote.Folders {
		fmt.Println(folder)
	}

	return nil
}

func handleRemoteFolderListAll(ctx *cli.Context, ctl *client.Client) error {
	remotes, err := ctl.RemoteLs()
	if err != nil {
		return err
	}

	for _, remote := range remotes {
		fmt.Println(remote.Name)
		for _, folder := range remote.Folders {
			fmt.Printf("  %s\n", folder)
		}
	}

	return nil
}

func handleNetLocate(ctx *cli.Context, ctl *client.Client) error {
	who := ctx.Args().First()
	timeoutSec, err := parseDuration(ctx.String("timeout"))
	if err != nil {
		return err
	}

	// Show a progress ticker, since the query might take quite long:
	progressTicker := time.NewTicker(500 * time.Millisecond)
	go func() {
		nDots := 0
		for range progressTicker.C {
			fmt.Printf("Scanning%-5s\r", strings.Repeat(".", nDots+1))
			nDots = (nDots + 1) % 5
		}
	}()

	candidateCh, err := ctl.NetLocate(who, ctx.String("mask"), timeoutSec)
	if err != nil {
		return fmt.Errorf("Failed to locate peers: %v", err)
	}

	somethingFound := false

	for candidate := range candidateCh {
		if !somethingFound {
			progressTicker.Stop()
			somethingFound = true

			// We can't use tabwriter here, sine it needs to update in realtime.
			// So we just fake it (badly) with printf-like formatting.
			fmt.Printf("%-30s %-10s %s\n", "NAME", "TYPE", "FINGERPRINT")
		}

		fingerprint := candidate.Fingerprint
		if fingerprint == "" {
			fingerprint = candidate.Addr + color.RedString(" (offline)")
		} else {
			fingerprint = color.GreenString(fingerprint)
		}

		fmt.Printf(
			"%-30s %-10s %s\n",
			candidate.Name,
			strings.Join(candidate.Mask, "|"),
			fingerprint,
		)
	}

	if !somethingFound {
		fmt.Println("No results. Maybe nobodoy is online?")
	}

	return nil
}

func handleRemotePing(ctx *cli.Context, ctl *client.Client) error {
	who := ctx.Args().First()

	msg := fmt.Sprintf("ping to %s: ", color.MagentaString(who))
	roundtrip, err := ctl.RemotePing(who)
	if err != nil {
		msg += color.RedString("✘")
		msg += fmt.Sprintf(" (%v)", err)
	} else {
		msg += color.GreenString("✔")
		msg += fmt.Sprintf(" (%3.5fs)", roundtrip)
	}

	fmt.Println(msg)
	return nil
}

func handlePin(ctx *cli.Context, ctl *client.Client) error {
	path := ctx.Args().First()
	return ctl.Pin(path)
}

func handleUnpin(ctx *cli.Context, ctl *client.Client) error {
	path := ctx.Args().First()
	return ctl.Unpin(path)
}

func handleWhoami(ctx *cli.Context, ctl *client.Client) error {
	self, err := ctl.Whoami()
	if err != nil {
		return err
	}

	printFingerprint := ctx.Bool("fingerprint")
	printName := ctx.Bool("name")

	userName := color.YellowString(self.CurrentUser)
	ownerName := color.GreenString(self.Owner)

	if !printFingerprint && !printName {
		if self.CurrentUser != self.Owner {
			fmt.Printf(
				"# Note: viewing %s's data currently\n",
				color.YellowString(userName),
			)
		}

		fmt.Printf("- Name: %s\n", color.YellowString(self.Owner))
		fmt.Printf("  Fingerprint: %s\n", self.Fingerprint)

		return nil
	}

	if printName {
		fmt.Printf("%s", ownerName)
	}

	if printFingerprint {
		if printName {
			fmt.Printf(" ")
		}
		fmt.Printf("%s", self.Fingerprint)
	}

	fmt.Printf("\n")
	return nil
}
