package store

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/boltdb/bolt"
	"github.com/disorganizer/brig/store/proto"
	"github.com/disorganizer/brig/util/ipfsutil"
	"github.com/disorganizer/brig/util/trie"
	protobuf "github.com/gogo/protobuf/proto"
	"github.com/jbenet/go-base58"
	"github.com/jbenet/go-multihash"
)

// TODO: Potential performance problem.
//       sync() can be slow when updating a file often.
//       Pool sync() therefore?

const (
	FileTypeInvalid = iota
	FileTypeRegular
	FileTypeDir
)

var (
	emptyChildren []*File
)

// ErrBadMap is returned by FromMap if the passed map is malformed.
type ErrBadMap struct {
	missing string
}

func (e ErrBadMap) Error() string {
	return fmt.Sprintf("can't import file: bad map, `%s` missing or wrong type.", e.missing)
}

type FileType int

// Metadata is the metadata that might change during modifications of the file.
// (key should not change)
type Metadata struct {
	// size is the file size in bytes.
	size int64

	// modTime is the time when the file or it's metadata was last changed.
	modTime time.Time

	// hash is the ipfs multihash of the file.
	hash *Hash

	// key is the key that was used to encrypt this file.
	key []byte

	// kind is the type of the file (type is reserved...)
	kind FileType
}

// File represents a single file in the repository.
// It stores all metadata about it and links to the actual data.
type File struct {
	*Metadata

	// Mutex protecting access to the trie.
	// Note that only one mutex exists per trie.
	*sync.RWMutex

	// Pointer to the store (for easy access)
	store *Store

	// trie.Node inside the Trie.
	// The file struct is stored as node.Data internally.
	node *trie.Node
}

func (f *File) insert(root *File, path string) {
	f.node = root.node.InsertWithData(path, f)
}

// Sync writes an up-to-date version of the file metadata to bolt.
// You probably do not need to call that yourself.
func (f *File) Sync() {
	f.Lock()
	defer f.Unlock()

	f.sync()
}

// UpdateSize updates the size (and therefore also the ModTime) of the file.
// The change is written to bolt.
func (f *File) UpdateSize(size int64) {
	f.Lock()
	defer f.Unlock()

	f.size = size
	f.modTime = time.Now()
	f.sync()
}

// Size returns the current size in a threadsafe manner.
func (f *File) Size() int64 {
	f.RLock()
	defer f.RUnlock()

	return int64(f.size)
}

// ModTime returns the current mtime in a threadsafe manner.
func (f *File) ModTime() time.Time {
	f.RLock()
	defer f.RUnlock()

	return f.modTime
}

// UpdateModTime safely updates the ModTime field of the file.
// The change is written to bolt.
func (f *File) UpdateModTime(modTime time.Time) {
	f.Lock()
	defer f.Unlock()

	f.modTime = modTime
	f.sync()
}

func (f *File) xorHash(hash *Hash) error {
	if f.kind != FileTypeDir {
		log.Warningf("Not a directory TODO")
		return nil
	}

	digest, err := multihash.Decode(hash.Multihash)
	if err != nil {
		return err
	}

	var ownHash []byte
	if f.hash == nil {
		ownHash = make([]byte, multihash.DefaultLengths[digest.Code])
	} else {
		ownDigest, err := multihash.Decode(f.hash.Multihash)
		if err != nil {
			return err
		}

		ownHash = ownDigest.Digest
	}

	for i := 0; i < len(ownHash); i++ {
		ownHash[i] ^= digest.Digest[i]
	}

	mhash, err := multihash.Encode(ownHash, digest.Code)
	if err != nil {
		log.Errorf("Unable to decode `%v` as multihash: %v", hash, err)
		return err
	}

	f.hash = &Hash{mhash}
	return nil
}

func (f *File) sync() {
	path := f.node.Path()
	log.Debugf("store-sync: %s (size: %d  mod: %v)", path, f.size, f.modTime)

	f.store.db.Update(withBucket("index", func(tx *bolt.Tx, bucket *bolt.Bucket) error {
		data, err := f.marshal()
		if err != nil {
			return err
		}

		if err := bucket.Put([]byte(path), data); err != nil {
			return err
		}

		return nil
	}))
}

// NewFile returns a file inside a repo.
// Path is relative to the repo root.
func NewFile(store *Store, path string) (*File, error) {
	// TODO: Make this configurable?
	key := make([]byte, 32)
	n, err := rand.Reader.Read(key)
	if err != nil {
		return nil, err
	}

	if n != 32 {
		return nil, fmt.Errorf("Read less than desired key size: %v", n)
	}

	now := time.Now()
	file := &File{
		store:   store,
		RWMutex: store.Root.RWMutex,
		Metadata: &Metadata{
			key:     key,
			kind:    FileTypeRegular,
			modTime: now,
		},
	}

	store.Root.Lock()
	defer store.Root.Unlock()

	file.insert(store.Root, path)

	file.node.Up(func(parentNode *trie.Node) {
		parent := parentNode.Data.(*File)
		if parent == file {
			return
		}

		parent.xorHash(file.hash)
		parent.Metadata.size += file.Metadata.size
		parent.Metadata.modTime = now
		parent.sync()
	})

	return file, nil
}

// NewDir returns a new empty directory File.
func NewDir(store *Store, path string) (*File, error) {
	store.Root.Lock()
	defer store.Root.Unlock()

	return newDirUnlocked(store, path)
}

func newDirUnlocked(store *Store, path string) (*File, error) {
	var mu *sync.RWMutex
	if store.Root == nil {
		// We're probably just called to create store.Root.
		mu = &sync.RWMutex{}
	} else {
		mu = store.Root.RWMutex
	}

	dir := &File{
		store:   store,
		RWMutex: mu,
		Metadata: &Metadata{
			modTime: time.Now(),
			kind:    FileTypeDir,
		},
	}

	if store.Root == nil {
		dir.node = trie.NewNodeWithData(dir)
	} else {
		dir.insert(store.Root, path)
	}

	return dir, nil
}

// Marshal converts a file to a protobuf-byte representation.
func (f *File) Marshal() ([]byte, error) {
	f.RLock()
	defer f.RUnlock()

	return f.marshal()
}

func (f *File) marshal() ([]byte, error) {
	modTimeStamp, err := f.modTime.MarshalText()
	if err != nil {
		return nil, err
	}

	dataFile := &proto.File{
		Path:     protobuf.String(f.node.Path()),
		Key:      f.key,
		FileSize: protobuf.Int64(f.size),
		ModTime:  modTimeStamp,
		Kind:     protobuf.Int32(int32(f.kind)),
		Hash:     f.hashUnlocked().Multihash,
	}

	data, err := protobuf.Marshal(dataFile)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Unmarshal decodes the data in `buf` and inserts the unmarshaled file
// into `store`.
func Unmarshal(store *Store, buf []byte) (*File, error) {
	dataFile := &proto.File{}
	if err := protobuf.Unmarshal(buf, dataFile); err != nil {
		return nil, err
	}

	modTimeStamp := &time.Time{}
	if err := modTimeStamp.UnmarshalText(dataFile.GetModTime()); err != nil {
		return nil, err
	}

	file := &File{
		store:   store,
		RWMutex: store.Root.RWMutex,
		Metadata: &Metadata{
			size:    dataFile.GetFileSize(),
			modTime: *modTimeStamp,
			hash:    &Hash{dataFile.GetHash()},
			key:     dataFile.GetKey(),
			kind:    FileType(dataFile.GetKind()),
		},
	}

	path := dataFile.GetPath()

	file.Lock()
	file.insert(store.Root, path)
	file.Unlock()

	return file, nil
}

///////////////////
// TRIE LIKE API //
///////////////////

// Root returns the uppermost node reachable from the receiver.
func (f *File) Root() *File {
	f.RLock()
	defer f.RUnlock()

	return f.store.Root
}

// Lookup searches for a node references by a path.
func (f *File) Lookup(path string) *File {
	f.RLock()
	defer f.RUnlock()

	node := f.node.Lookup(path)
	if node != nil {
		return node.Data.(*File)
	}

	return nil
}

// Remove removes the node at path and all of it's children.
// The parent of the removed node is returned, which might be nil.
func (f *File) Remove() {
	f.Lock()
	defer f.Unlock()

	// Remove from trie:
	f.node.Remove()
}

// Len returns the current number of elements in the trie.
// This counts only explicitly inserted Nodes.
func (f *File) Len() int64 {
	f.RLock()
	defer f.RUnlock()

	return f.node.Len()
}

// Up goes up in the hierarchy and calls `visit` on each visited node.
func (f *File) Up(visit func(*File)) {
	f.RLock()
	defer f.RUnlock()

	f.node.Up(func(parent *trie.Node) {
		file := parent.Data.(*File)
		visit(file)
	})
}

// Kind returns the FileType
func (f *File) Kind() FileType {
	f.RLock()
	defer f.RUnlock()

	return f.kind
}

// Path returns the absolute path of the file inside the repository, starting with /.
func (f *File) Path() string {
	f.RLock()
	defer f.RUnlock()

	return f.node.Path()
}

// Walk recursively calls `visit` on each child and f itself.
// If `dfs` is true, the order will be depth-first, otherwise breadth-first.
func (f *File) Walk(dfs bool, visit func(*File) bool) {
	f.RLock()
	defer f.RUnlock()

	f.node.Walk(dfs, func(n *trie.Node) bool {
		fmt.Printf("Visit %s %p\n", n.Path(), n.Data)
		return visit(n.Data.(*File))
	})
}

// Children returns a list of children of the
func (f *File) Children() []*File {
	f.RLock()
	defer f.RUnlock()

	// Optimization: Return the same empty slice for leaf nodes.
	n := len(f.node.Children)
	if n == 0 {
		return emptyChildren
	}

	children := make([]*File, 0, n)
	for _, child := range f.node.Children {
		if child.Data != nil {
			children = append(children, child.Data.(*File))
		}
	}

	return children
}

// Child returns the direct child of the receiver called `name` or nil
func (f *File) Child(name string) *File {
	f.RLock()
	defer f.RUnlock()

	if f.node.Children == nil {
		return nil
	}

	child, ok := f.node.Children[name]
	if ok {
		return child.Data.(*File)
	}

	return nil
}

// Name returns the basename of the file.
func (f *File) Name() string {
	f.RLock()
	defer f.RUnlock()

	return f.node.Name
}

// Stream opens a reader that yields the raw data of the file,
// already transparently decompressed and decrypted.
func (f *File) Stream() (ipfsutil.Reader, error) {
	f.RLock()
	defer f.RUnlock()

	log.Debugf("Stream `%s` (hash: %s) (key: %x)", f.node.Path(), f.hash.B58String(), f.key)

	ipfsNode, err := f.store.IpfsNode()
	if err != nil {
		return nil, err
	}

	ipfsStream, err := ipfsutil.Cat(ipfsNode, f.hash.Multihash)
	if err != nil {
		return nil, err
	}

	return NewIpfsReader(f.key, ipfsStream)
}

// Parent returns the parent directory of File.
// If `f` is already the root, it will return itself (and never nil).
func (f *File) Parent() *File {
	f.RLock()
	defer f.RUnlock()

	parent := f.node.Parent
	if parent != nil {
		return parent.Data.(*File)
	}

	return f
}

// Hash returns the hash of a file. If it is leaf file,
// the hash is returned directly; directory hashes
// are computed by combining the child hashes.
func (f *File) Hash() *Hash {
	f.RLock()
	defer f.RUnlock()

	return f.hashUnlocked()
}

func (f *File) hashUnlocked() *Hash {
	if f.kind == FileTypeRegular {
		if !f.hash.Valid() {
			log.Warningf("file-hash: BUG: File with no hash: %v", f.node.Path())
		}

		return f.hash
	}

	if f.hash.Valid() {
		// Directory has children with valid hashes:
		return f.hash
	}

	// Take a lucky guess:
	code := multihash.BLAKE2S
	mhash, err := multihash.Encode(make([]byte, multihash.DefaultLengths[code]), code)
	if err != nil {
		// TODO: check if this is a good idea at all.
		log.Errorf("Oops")
		return nil
	}

	return &Hash{mhash}
}

// Key returns the encryption key.
func (f *File) Key() []byte {
	f.RLock()
	defer f.RUnlock()

	return f.key
}

func (f *File) MarshalJSON() ([]byte, error) {
	f.RLock()
	defer f.RUnlock()

	path := f.node.Path()
	history, err := f.store.History(path)
	if err != nil {
		return nil, err
	}

	// Usual marshalling does not work well for *File,
	// since it contains recursive data and some data
	// is stored only implicitly (e.g. Path())
	data := map[string]interface{}{
		"size":    f.size,
		"modtime": f.modTime,
		"hash":    f.hashUnlocked().B58String(),
		"path":    path,
		"kind":    f.kind,
	}

	if history != nil {
		data["history"] = history
	}

	if f.key != nil {
		data["key"] = base58.Encode(f.key)
	}

	return json.MarshalIndent(data, "", "\t")
}

// TODO: This sucks badly.
func FromMap(store *Store, m map[string]interface{}) (*File, error) {
	fmt.Println("IMPORT", m)
	path, ok := m["path"].(string)
	if !ok {
		return nil, ErrBadMap{"path"}
	}

	modTimeStr, ok := m["modtime"].(string)
	if !ok {
		return nil, ErrBadMap{"modtime"}
	}

	modTime := time.Time{}
	if err := modTime.UnmarshalText([]byte(modTimeStr)); err != nil {
		return nil, ErrBadMap{"modtime"}
	}

	size, ok := m["size"].(float64)
	if !ok {
		return nil, ErrBadMap{"size"}
	}

	kind, ok := m["kind"].(float64)
	if !ok {
		return nil, ErrBadMap{"kind"}
	}

	keyStr, ok := m["key"].(string)
	if !ok {
		// Missing key is OK for directories.
		// TODO: check kind.
		if kind == FileTypeRegular {
			return nil, fmt.Errorf("Regular file without key received: %s", path)
		}

		keyStr = ""
	}

	key := base58.Decode(keyStr)

	hashB58Str, ok := m["hash"].(string)
	if !ok {
		fmt.Println("Fail to unmarshal", hashB58Str)
		return nil, ErrBadMap{"hash"}
	}

	hash, err := multihash.FromB58String(hashB58Str)
	if err != nil {
		fmt.Println("Fail to parse", err, hashB58Str)
		return nil, ErrBadMap{"hash"}
	}

	newMeta := &Metadata{
		modTime: modTime,
		size:    int64(size),
		kind:    FileType(kind),
		hash:    &Hash{hash},
		key:     key,
	}

	// Check if we know this file already:
	file := store.Root.Lookup(path)
	if file == nil {
		// No such file yet. Create new.
		file, err = NewFile(store, path)
		if err != nil {
			return nil, err
		}

		// TODO: Import history.
	} else {
		// Previous version here, create checkpoint before updating metadata.
		err = store.MakeCheckpoint(file.Metadata, newMeta, path, file.Path())
		if err != nil {
			return nil, err
		}
	}

	file.Metadata = newMeta
	return file, nil
}
