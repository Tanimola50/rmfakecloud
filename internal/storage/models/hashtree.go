package models

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"

	log "github.com/sirupsen/logrus"
)

const schemaVersion = "3"
const docType = "80000000"
const fileType = "0"
const delimiter = ':'

func HashEntries(entries []*HashEntry) (string, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntryName < entries[j].EntryName })
	hasher := sha256.New()
	for _, d := range entries {
		//TODO: back and forth converting
		bh, err := hex.DecodeString(d.Hash)
		if err != nil {
			return "", err
		}
		hasher.Write(bh)
	}
	hash := hasher.Sum(nil)
	hashStr := hex.EncodeToString(hash)
	return hashStr, nil
}

func Hash(r io.Reader) (string, int64, error) {
	hasher := sha256.New()
	w, err := io.Copy(hasher, r)
	if err != nil {
		return "", w, err
	}
	h := hasher.Sum(nil)
	hstr := hex.EncodeToString(h)
	return hstr, w, err
}
func FileHashAndSize(file string) ([]byte, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	hasher := sha256.New()
	io.Copy(hasher, f)
	h := hasher.Sum(nil)
	size, err := f.Seek(0, io.SeekEnd)
	return h, size, err
}

// LoadTree loads
func LoadTree(cacheFile string) (*HashTree, error) {
	tree := HashTree{}
	if _, err := os.Stat(cacheFile); err == nil {
		b, err := ioutil.ReadFile(cacheFile)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(b, &tree)
		if err != nil {
			log.Println("cache corrupt")
			return nil, err
		}
		log.Println("Cache loaded: ", cacheFile)
	}

	return &tree, nil
}

// Save saves
func (t *HashTree) Save(cacheFile string) error {
	log.Println("Writing cache: ", cacheFile)
	b, err := json.MarshalIndent(t, "", "")
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(cacheFile, b, 0644)
	return err
}

func parseEntry(line string) (*HashEntry, error) {
	entry := HashEntry{}
	rdr := NewFieldReader(line)
	numFields := len(rdr.fields)
	if numFields != 5 {
		return nil, fmt.Errorf("wrong number of fields %d", numFields)
	}
	var err error
	entry.Hash, err = rdr.Next()
	if err != nil {
		return nil, err
	}
	entry.Type, err = rdr.Next()
	if err != nil {
		return nil, err
	}
	entry.EntryName, err = rdr.Next()
	if err != nil {
		return nil, err
	}
	tmp, err := rdr.Next()
	if err != nil {
		return nil, err
	}
	entry.Subfiles, err = strconv.Atoi(tmp)
	if err != nil {
		return nil, fmt.Errorf("cannot read subfiles %s %v", line, err)
	}
	tmp, err = rdr.Next()
	if err != nil {
		return nil, err
	}
	entry.Size, err = strconv.ParseInt(tmp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cannot read size %s %v", line, err)
	}
	return &entry, nil
}

func parseIndex(f io.Reader) ([]*HashEntry, error) {
	var entries []*HashEntry
	scanner := bufio.NewScanner(f)
	scanner.Scan()
	schema := scanner.Text()

	if schema != schemaVersion {
		return nil, errors.New("wrong schema")
	}
	for scanner.Scan() {
		line := scanner.Text()
		entry, err := parseEntry(line)
		if err != nil {
			return nil, fmt.Errorf("cant parse line '%s', %w", line, err)
		}

		entries = append(entries, entry)
	}
	return entries, nil
}

// RootIndex reads the root index
func (t *HashTree) RootIndex() (io.ReadCloser, error) {
	pipeReader, pipeWriter := io.Pipe()
	w := bufio.NewWriter(pipeWriter)
	go func() {
		defer pipeWriter.Close()
		w.WriteString(schemaVersion)
		w.WriteString("\n")
		for _, d := range t.Docs {
			w.WriteString(d.Line())
			w.WriteString("\n")
		}
		w.Flush()
	}()

	return pipeReader, nil
}

// HashTree a syncing concept for faster diffing
type HashTree struct {
	Hash       string
	Generation int64
	Docs       []*HashDoc
}

// FindDoc finds a document by its name
func (t *HashTree) FindDoc(documentID string) (*HashDoc, error) {
	//O(n)
	for _, d := range t.Docs {
		if d.EntryName == documentID {
			return d, nil
		}
	}
	return nil, fmt.Errorf("doc %s not found", documentID)
}

// Remove removes
func (t *HashTree) Remove(documentID string) error {
	docIndex := -1
	for index, d := range t.Docs {
		if d.EntryName == documentID {
			docIndex = index
			break
		}
	}
	if docIndex > -1 {
		log.Infof("Removing %s", documentID)
		length := len(t.Docs) - 1
		t.Docs[docIndex] = t.Docs[length]
		t.Docs = t.Docs[:length]

		t.Rehash()
		return nil
	}
	return fmt.Errorf("%s not found", documentID)
}

// Rehash recalcualte the root hash from all docs
func (t *HashTree) Rehash() error {
	entries := []*HashEntry{}
	for _, e := range t.Docs {
		entries = append(entries, &e.HashEntry)
	}
	hash, err := HashEntries(entries)
	if err != nil {
		return err
	}
	log.Println("New root hash: ", hash)
	t.Hash = hash
	return nil
}

// Mirror makes the tree look like the storage
func (t *HashTree) Mirror(r RemoteStorage) (changed bool, err error) {
	rootHash, gen, err := r.GetRootIndex()
	if err != nil {
		return
	}
	if rootHash == "" && gen == 0 {
		log.Println("Empty cloud")
		t.Docs = nil
		t.Generation = 0
		return
	}

	if rootHash == t.Hash {
		if gen != t.Generation {
			t.Generation = gen
			return true, nil
		}
		return
	}
	log.Printf("remote root hash different")

	rdr, err := r.GetReader(rootHash)
	if err != nil {
		return
	}
	defer rdr.Close()

	entries, err := parseIndex(rdr)
	if err != nil {
		return
	}

	head := make([]*HashDoc, 0)
	current := make(map[string]*HashDoc)
	new := make(map[string]*HashEntry)
	for _, e := range entries {
		new[e.EntryName] = e
	}
	//current documents
	for _, doc := range t.Docs {
		if entry, ok := new[doc.HashEntry.EntryName]; ok {
			//hash different update
			if entry.Hash != doc.Hash {
				log.Println("doc updated: " + doc.EntryName)
				doc.Mirror(entry, r)
			}
			head = append(head, doc)
			current[doc.EntryName] = doc
		}

	}

	//find new entries
	for k, newEntry := range new {
		if _, ok := current[k]; !ok {
			doc := &HashDoc{}
			log.Println("doc new: " + k)
			doc.Mirror(newEntry, r)
			head = append(head, doc)
		}
	}
	sort.Slice(head, func(i, j int) bool { return head[i].EntryName < head[j].EntryName })
	t.Docs = head
	t.Generation = gen
	t.Hash = rootHash
	return true, nil
}

// BuildTree from remote storage
func BuildTree(provider RemoteStorage) (*HashTree, error) {
	tree := HashTree{}

	rootHash, gen, err := provider.GetRootIndex()

	if err != nil {
		return nil, err
	}
	tree.Hash = rootHash
	tree.Generation = gen

	rootIndex, err := provider.GetReader(rootHash)
	if err != nil {
		return nil, err
	}

	defer rootIndex.Close()
	entries, _ := parseIndex(rootIndex)

	for _, e := range entries {
		f, _ := provider.GetReader(e.Hash)
		defer f.Close()

		doc := &HashDoc{}
		doc.HashEntry = *e
		tree.Docs = append(tree.Docs, doc)

		items, _ := parseIndex(f)
		doc.Files = items
		for _, i := range items {
			doc.ReadMetadata(i, provider)
		}
	}

	return &tree, nil
}
