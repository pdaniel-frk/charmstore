// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charmstore

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/juju/loggo"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/charm.v4"
	"gopkg.in/macaroon-bakery.v0/bakery"
	"gopkg.in/macaroon-bakery.v0/bakery/mgostorage"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"

	"github.com/juju/charmstore/internal/blobstore"
	"github.com/juju/charmstore/internal/mongodoc"
	"github.com/juju/charmstore/params"
)

var logger = loggo.GetLogger("charmstore.internal.charmstore")

// Store represents the underlying charm and blob data stores.
type Store struct {
	DB        StoreDatabase
	BlobStore *blobstore.Store
	ES        *SearchIndex
	Bakery    *bakery.Service

	// Cache for statistics key words (two generations).
	cacheMu       sync.RWMutex
	statsIdNew    map[string]int
	statsIdOld    map[string]int
	statsTokenNew map[int]string
	statsTokenOld map[int]string
}

// NewStore returns a Store that uses the given database
// and search index. If bakeryParams is not nil,
// the Bakery field in the resulting Store will be set
// to a new Service that stores macaroons in mongo.
func NewStore(db *mgo.Database, si *SearchIndex, bakeryParams *bakery.NewServiceParams) (*Store, error) {
	s := &Store{
		DB:        StoreDatabase{db},
		BlobStore: blobstore.New(db, "entitystore"),
		ES:        si,
	}
	if err := s.ensureIndexes(); err != nil {
		return nil, errgo.Notef(err, "cannot ensure indexes")
	}
	if err := s.ES.ensureIndexes(false); err != nil {
		return nil, errgo.Notef(err, "cannot ensure elasticsearch indexes")
	}
	if bakeryParams != nil {
		macStore, err := mgostorage.New(s.DB.Macaroons())
		if err != nil {
			return nil, errgo.Notef(err, "cannot create macaroon store")
		}
		p := *bakeryParams
		p.Store = macStore
		bsvc, err := bakery.NewService(p)
		if err != nil {
			return nil, errgo.Notef(err, "cannot make bakery service")
		}
		s.Bakery = bsvc
	}
	go func() {
		if err := s.syncSearch(); err != nil {
			logger.Errorf("Cannot populate elasticsearch: %v", err)
		}
	}()
	return s, nil
}

func (s *Store) ensureIndexes() error {
	indexes := []struct {
		c *mgo.Collection
		i mgo.Index
	}{{
		s.DB.StatCounters(),
		mgo.Index{Key: []string{"k", "t"}, Unique: true},
	}, {
		s.DB.StatTokens(),
		mgo.Index{Key: []string{"t"}, Unique: true},
	}, {
		s.DB.Entities(),
		mgo.Index{Key: []string{"baseurl"}},
	}, {
		s.DB.Entities(),
		mgo.Index{Key: []string{"uploadtime"}},
	}, {
		s.DB.BaseEntities(),
		mgo.Index{Key: []string{"public"}},
	}, {
		s.DB.Logs(),
		mgo.Index{Key: []string{"urls"}},
	}}
	for _, idx := range indexes {
		err := idx.c.EnsureIndex(idx.i)
		if err != nil {
			return errgo.Mask(err)
		}
	}
	return nil
}

func (s *Store) putArchive(archive blobstore.ReadSeekCloser) (blobName, blobHash string, size int64, err error) {
	hash := blobstore.NewHash()
	size, err = io.Copy(hash, archive)
	if err != nil {
		return "", "", 0, errgo.Mask(err)
	}
	if _, err = archive.Seek(0, 0); err != nil {
		return "", "", 0, errgo.Mask(err)
	}
	blobHash = fmt.Sprintf("%x", hash.Sum(nil))
	blobName = bson.NewObjectId().Hex()
	if err = s.BlobStore.PutUnchallenged(archive, blobName, size, blobHash); err != nil {
		return "", "", 0, errgo.Mask(err)
	}
	return blobName, blobHash, size, nil
}

// AddCharmWithArchive is like AddCharm but
// also adds the charm archive to the blob store.
// This method is provided principally so that
// tests can easily create content in the store.
func (s *Store) AddCharmWithArchive(url *charm.Reference, ch charm.Charm) error {
	blobName, blobHash, blobSize, err := s.uploadCharmOrBundle(ch)
	if err != nil {
		return errgo.Mask(err)
	}
	return s.AddCharm(ch, AddParams{
		URL:      url,
		BlobName: blobName,
		BlobHash: blobHash,
		BlobSize: blobSize,
	})
}

// AddBundleWithArchive is like AddBundle but
// also adds the charm archive to the blob store.
// This method is provided principally so that
// tests can easily create content in the store.
func (s *Store) AddBundleWithArchive(url *charm.Reference, b charm.Bundle) error {
	blobName, blobHash, size, err := s.uploadCharmOrBundle(b)
	if err != nil {
		return errgo.Mask(err)
	}
	return s.AddBundle(b, AddParams{
		URL:      url,
		BlobName: blobName,
		BlobHash: blobHash,
		BlobSize: size,
	})
}

func (s *Store) uploadCharmOrBundle(c interface{}) (blobName, blobHash string, size int64, err error) {
	archive, err := getArchive(c)
	if err != nil {
		return "", "", 0, errgo.Mask(err)
	}
	defer archive.Close()
	return s.putArchive(archive)
}

// AddParams holds parameters held in common between the
// Store.AddCharm and Store.AddBundle methods.
type AddParams struct {
	// URL holds the id to be associated with the stored entity.
	URL *charm.Reference

	// BlobName holds the name of the entity's archive blob.
	BlobName string

	// BlobHash holds the hash of the entity's archive blob.
	BlobHash string

	// BlobHash holds the size of the entity's archive blob.
	BlobSize int64

	// Contents holds references to files inside the
	// entity's archive blob.
	Contents map[mongodoc.FileId]mongodoc.ZipFile

	// PromulgatedURL holds the promulgated URL of the entity. If the entity
	// is not promulgated this should be set to nil.
	PromulgatedURL *charm.Reference

	// PromulgatedRevision holds the revision number from the promulgated URL.
	// If the entity is not promulgated this should be set to -1.
	PromulgatedRevision int
}

// AddCharm adds a charm entities collection with the given
// parameters.
func (s *Store) AddCharm(c charm.Charm, p AddParams) (err error) {
	if p.URL.Series == "bundle" {
		return errgo.Newf("charm added with invalid id %v", p.URL)
	}
	entity := &mongodoc.Entity{
		URL:                     p.URL,
		BaseURL:                 baseURL(p.URL),
		User:                    p.URL.User,
		Name:                    p.URL.Name,
		Revision:                p.URL.Revision,
		Series:                  p.URL.Series,
		BlobHash:                p.BlobHash,
		BlobName:                p.BlobName,
		Size:                    p.BlobSize,
		UploadTime:              time.Now(),
		CharmMeta:               c.Meta(),
		CharmConfig:             c.Config(),
		CharmActions:            c.Actions(),
		CharmProvidedInterfaces: interfacesForRelations(c.Meta().Provides),
		CharmRequiredInterfaces: interfacesForRelations(c.Meta().Requires),
		Contents:                p.Contents,
		PromulgatedURL:          p.PromulgatedURL,
		PromulgatedRevision:     p.PromulgatedRevision,
	}

	// Check that we're not going to create a charm that duplicates
	// the name of a bundle. This is racy, but it's the best we can do.
	entities, err := s.FindEntities(baseURL(p.URL))
	if err != nil {
		return errgo.Notef(err, "cannot check for existing entities")
	}
	for _, entity := range entities {
		if entity.URL.Series == "bundle" {
			return errgo.Newf("charm name duplicates bundle name %v", entity.URL)
		}
	}
	if err := s.insertEntity(entity); err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrDuplicateUpload))
	}
	return nil
}

var everyonePerm = []string{params.Everyone}

func (s *Store) insertEntity(entity *mongodoc.Entity) (err error) {
	readPerm := everyonePerm
	var writePerm []string
	if entity.User != "" {
		readPerm = []string{params.Everyone, entity.User}
		writePerm = []string{entity.User}
	}
	// Add the base entity to the database.
	baseEntity := &mongodoc.BaseEntity{
		URL:  entity.BaseURL,
		User: entity.User,
		Name: entity.Name,
		// TODO frankban: allow specifying non-public charms on initial upload.
		Public: true,
		ACLs: mongodoc.ACL{
			Read:  readPerm,
			Write: writePerm,
		},
		Promulgated: entity.PromulgatedURL != nil,
	}
	err = s.DB.BaseEntities().Insert(baseEntity)
	if err != nil && !mgo.IsDup(err) {
		return errgo.Mask(err)
	}

	// Add the entity to the database.
	err = s.DB.Entities().Insert(entity)
	if mgo.IsDup(err) {
		return params.ErrDuplicateUpload
	}
	if err != nil {
		return errgo.Mask(err)
	}
	// Ensure that if anything fails after this, that we delete
	// the entity, otherwise we will be left in an internally
	// inconsistent state.
	defer func() {
		if err != nil {
			if err := s.DB.Entities().RemoveId(entity.URL); err != nil {
				logger.Errorf("cannot remove entity after elastic search failure: %v", err)
			}
		}
	}()
	// Add entity to ElasticSearch.
	if err := s.UpdateSearch(entity.URL); err != nil {
		return errgo.Notef(err, "cannot index %s to ElasticSearch", entity.URL)
	}
	return nil
}

// FindEntity finds the entity in the store with the given URL,
// which must be fully qualified. If any fields are specified,
// only those fields will be populated in the returned entities.
func (s *Store) FindEntity(url *charm.Reference, fields ...string) (*mongodoc.Entity, error) {
	if url.Series == "" || url.Revision == -1 {
		return nil, errgo.Newf("entity id %q is not fully qualified", url)
	}
	entities, err := s.FindEntities(url, fields...)
	if err != nil {
		return nil, errgo.Mask(err)
	}
	if len(entities) == 0 {
		return nil, errgo.WithCausef(nil, params.ErrNotFound, "entity not found")
	}
	// The URL is guaranteed to be fully qualified so we'll always
	// get exactly one result.
	return entities[0], nil
}

// FindEntities finds all entities in the store matching the given URL.
// If any fields are specified, only those fields will be
// populated in the returned entities.
func (s *Store) FindEntities(url *charm.Reference, fields ...string) ([]*mongodoc.Entity, error) {
	var q bson.D
	if url.Series == "" || url.Revision == -1 {
		// The url can match several entities - select
		// based on the base URL and filter afterwards.
		q = bson.D{{"baseurl", baseURL(url)}}
	} else {
		q = bson.D{{"_id", url}}
	}

	query := selectFields(s.DB.Entities().Find(q), fields)
	var docs []*mongodoc.Entity
	err := query.All(&docs)
	if err != nil {
		return nil, errgo.Mask(err)
	}
	last := 0
	for _, doc := range docs {
		if matchURL(doc.URL, url) {
			docs[last] = doc
			last++
		}
	}
	return docs[0:last], nil
}

// FindBaseEntity finds the base entity in the store using the given URL,
// which can either represent a fully qualified entity or a base id.
// If any fields are specified, only those fields will be populated in the
// returned base entity.
func (s *Store) FindBaseEntity(url *charm.Reference, fields ...string) (*mongodoc.BaseEntity, error) {
	query := s.DB.BaseEntities().FindId(baseURL(url))
	query = selectFields(query, fields)
	var baseEntity mongodoc.BaseEntity
	if err := query.One(&baseEntity); err != nil {
		if err == mgo.ErrNotFound {
			return nil, errgo.WithCausef(nil, params.ErrNotFound, "base entity not found")
		}
		return nil, errgo.Mask(err)
	}
	return &baseEntity, nil
}

func selectFields(query *mgo.Query, fields []string) *mgo.Query {
	if len(fields) > 0 {
		sel := make(bson.D, len(fields))
		for i, field := range fields {
			sel[i] = bson.DocElem{field, 1}
		}
		query = query.Select(sel)
	}
	return query
}

// ExpandURL returns all the URLs that the given URL may refer to.
func (s *Store) ExpandURL(url *charm.Reference) ([]*charm.Reference, error) {
	entities, err := s.FindEntities(url, "_id")
	if err != nil {
		return nil, errgo.Mask(err)
	}
	urls := make([]*charm.Reference, len(entities))
	for i, entity := range entities {
		urls[i] = entity.URL
	}
	return urls, nil
}

func matchURL(url, pattern *charm.Reference) bool {
	if pattern.Series != "" && url.Series != pattern.Series {
		return false
	}
	if pattern.Revision != -1 && url.Revision != pattern.Revision {
		return false
	}
	// Check the name for completness only - the
	// query should only be returning URLs with
	// matching names.
	return url.Name == pattern.Name
}

func interfacesForRelations(rels map[string]charm.Relation) []string {
	// Eliminate duplicates by storing interface names into a map.
	interfaces := make(map[string]bool)
	for _, rel := range rels {
		interfaces[rel.Interface] = true
	}
	result := make([]string, 0, len(interfaces))
	for iface := range interfaces {
		result = append(result, iface)
	}
	return result
}

func baseURL(url *charm.Reference) *charm.Reference {
	newURL := *url
	newURL.Revision = -1
	newURL.Series = ""
	return &newURL
}

var errNotImplemented = errgo.Newf("not implemented")

// AddBundle adds a bundle to the entities collection with the given
// parameters.
func (s *Store) AddBundle(b charm.Bundle, p AddParams) error {
	if p.URL.Series != "bundle" {
		return errgo.Newf("bundle added with invalid id %v", p.URL)
	}
	bundleData := b.Data()
	urls, err := bundleCharms(bundleData)
	if err != nil {
		return errgo.Mask(err)
	}
	entity := &mongodoc.Entity{
		URL:                 p.URL,
		BaseURL:             baseURL(p.URL),
		User:                p.URL.User,
		Name:                p.URL.Name,
		Revision:            p.URL.Revision,
		Series:              p.URL.Series,
		BlobHash:            p.BlobHash,
		BlobName:            p.BlobName,
		Size:                p.BlobSize,
		UploadTime:          time.Now(),
		BundleData:          bundleData,
		BundleUnitCount:     newInt(bundleUnitCount(bundleData)),
		BundleMachineCount:  newInt(bundleMachineCount(bundleData)),
		BundleReadMe:        b.ReadMe(),
		BundleCharms:        urls,
		Contents:            p.Contents,
		PromulgatedURL:      p.PromulgatedURL,
		PromulgatedRevision: p.PromulgatedRevision,
	}

	// Check that we're not going to create a bundle that duplicates
	// the name of a charm. This is racy, but it's the best we can do.
	entities, err := s.FindEntities(baseURL(p.URL))
	if err != nil {
		return errgo.Notef(err, "cannot check for existing entities")
	}
	for _, entity := range entities {
		if entity.URL.Series != "bundle" {
			return errgo.Newf("bundle name duplicates charm name %s", entity.URL)
		}
	}
	if err := s.insertEntity(entity); err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrDuplicateUpload))
	}
	return nil
}

// OpenBlob opens a blob given its entity id; it returns the blob's
// data source, its size and its hash. It returns a params.ErrNotFound
// error if the entity does not exist.
func (s *Store) OpenBlob(id *charm.Reference) (r blobstore.ReadSeekCloser, size int64, hash string, err error) {
	blobName, hash, err := s.BlobNameAndHash(id)
	if err != nil {
		return nil, 0, "", errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	r, size, err = s.BlobStore.Open(blobName)
	if err != nil {
		return nil, 0, "", errgo.Notef(err, "cannot open archive data for %s", id)
	}
	return r, size, hash, nil
}

// BlobNameAndHash returns the name that is used to store the blob
// for the entity with the given id and its hash. It returns a params.ErrNotFound
// error if the entity does not exist.
func (s *Store) BlobNameAndHash(id *charm.Reference) (name, hash string, err error) {
	var entity mongodoc.Entity
	if err := s.DB.Entities().
		FindId(id).
		Select(bson.D{{"blobname", 1}, {"blobhash", 1}}).
		One(&entity); err != nil {
		if err == mgo.ErrNotFound {
			return "", "", errgo.WithCausef(nil, params.ErrNotFound, "entity not found")
		}
		return "", "", errgo.Notef(err, "cannot get %s", id)
	}
	return entity.BlobName, entity.BlobHash, nil
}

// OpenCachedBlobFile opens a file from the given entity's archive blob.
// The file is identified by the provided fileId. If the file has not
// previously been opened on this entity, the isFile function will be
// used to determine which file in the zip file to use. The result will
// be cached for the next time.
//
// When retrieving the entity, at least the BlobName and
// Contents fields must be populated.
func (s *Store) OpenCachedBlobFile(
	entity *mongodoc.Entity,
	fileId mongodoc.FileId,
	isFile func(f *zip.File) bool,
) (_ io.ReadCloser, err error) {
	if entity.BlobName == "" {
		// We'd like to check that the Contents field was populated
		// here but we can't because it doesn't necessarily
		// exist in the entity.
		return nil, errgo.New("provided entity does not have required fields")
	}
	zipf, ok := entity.Contents[fileId]
	if ok && !zipf.IsValid() {
		return nil, errgo.WithCausef(nil, params.ErrNotFound, "")
	}
	blob, size, err := s.BlobStore.Open(entity.BlobName)
	if err != nil {
		return nil, errgo.Notef(err, "cannot open archive blob")
	}
	defer func() {
		// When there's an error, we want to close
		// the blob, otherwise we need to keep the blob
		// open because it's used by the returned Reader.
		if err != nil {
			blob.Close()
		}
	}()
	if !ok {
		// We haven't already searched the archive for the icon,
		// so find its archive now.
		zipf, err = s.findZipFile(blob, size, isFile)
		if err != nil && errgo.Cause(err) != params.ErrNotFound {
			return nil, errgo.Mask(err)
		}
	}
	// We update the content entry regardless of whether we've
	// found a file, so that the next time that serveIcon is called
	// it can know that we've already looked.
	err = s.DB.Entities().UpdateId(
		entity.URL,
		bson.D{{"$set",
			bson.D{{"contents." + string(fileId), zipf}},
		}},
	)
	if err != nil {
		return nil, errgo.Notef(err, "cannot update %q", entity.URL)
	}
	if !zipf.IsValid() {
		// We searched for the file and didn't find it.
		return nil, errgo.WithCausef(nil, params.ErrNotFound, "")
	}

	// We know where the icon is stored. Now serve it up.
	r, err := ZipFileReader(blob, zipf)
	if err != nil {
		return nil, errgo.Notef(err, "cannot make zip file reader")
	}
	// We return a ReadCloser that reads from the newly created
	// zip file reader, but when closed, will close the originally
	// opened blob.
	return struct {
		io.Reader
		io.Closer
	}{r, blob}, nil
}

func (s *Store) findZipFile(blob io.ReadSeeker, size int64, isFile func(f *zip.File) bool) (mongodoc.ZipFile, error) {
	zipReader, err := zip.NewReader(&readerAtSeeker{blob}, size)
	if err != nil {
		return mongodoc.ZipFile{}, errgo.Notef(err, "cannot read archive data")
	}
	for _, f := range zipReader.File {
		if isFile(f) {
			return NewZipFile(f)
		}
	}
	return mongodoc.ZipFile{}, params.ErrNotFound
}

func newInt(x int) *int {
	return &x
}

// bundleUnitCount returns the number of units created by the bundle.
func bundleUnitCount(b *charm.BundleData) int {
	count := 0
	for _, service := range b.Services {
		count += service.NumUnits
	}
	return count
}

// bundleMachineCount returns the number of machines
// that will be created or used by the bundle.
func bundleMachineCount(b *charm.BundleData) int {
	count := len(b.Machines)
	for _, service := range b.Services {
		// The default placement is "new".
		placement := &charm.UnitPlacement{
			Machine: "new",
		}
		// Check for "new" placements, which means a new machine
		// must be added.
		for _, location := range service.To {
			var err error
			placement, err = charm.ParsePlacement(location)
			if err != nil {
				// Ignore invalid placements - a bundle should always
				// be verified before adding to the charm store so this
				// should never happen in practice.
				continue
			}
			if placement.Machine == "new" {
				count++
			}
		}
		// If there are less elements in To than NumUnits, the last placement
		// element is replicated. For this reason, if the last element is
		// "new", we need to add more machines.
		if placement != nil && placement.Machine == "new" {
			count += service.NumUnits - len(service.To)
		}
	}
	return count
}

// bundleCharms returns all the charm URLs used by a bundle,
// without duplicates.
func bundleCharms(data *charm.BundleData) ([]*charm.Reference, error) {
	// Use a map to de-duplicate the URL list: a bundle can include services
	// deployed by the same charm.
	urlMap := make(map[string]*charm.Reference)
	for _, service := range data.Services {
		url, err := charm.ParseReference(service.Charm)
		if err != nil {
			return nil, errgo.Mask(err)
		}
		urlMap[url.String()] = url
		// Also add the corresponding base URL.
		base := baseURL(url)
		urlMap[base.String()] = base
	}
	urls := make([]*charm.Reference, 0, len(urlMap))
	for _, url := range urlMap {
		urls = append(urls, url)
	}
	return urls, nil
}

// AddLog adds a log message to the database.
func (s *Store) AddLog(data *json.RawMessage, logLevel mongodoc.LogLevel, logType mongodoc.LogType, urls []*charm.Reference) error {
	// Encode the JSON data.
	b, err := json.Marshal(data)
	if err != nil {
		return errgo.Notef(err, "cannot marshal log data")
	}

	// Add the base URLs to the list of references associated with the log.
	// Also remove duplicate URLs while maintaining the references' order.
	var allUrls []*charm.Reference
	urlMap := make(map[string]bool)
	for _, url := range urls {
		urlStr := url.String()
		if ok, _ := urlMap[urlStr]; !ok {
			urlMap[urlStr] = true
			allUrls = append(allUrls, url)
		}
		base := baseURL(url)
		urlStr = base.String()
		if ok, _ := urlMap[urlStr]; !ok {
			urlMap[urlStr] = true
			allUrls = append(allUrls, base)
		}
	}

	// Add the log to the database.
	log := &mongodoc.Log{
		Data:  b,
		Level: logLevel,
		Type:  logType,
		URLs:  allUrls,
		Time:  time.Now(),
	}
	if err := s.DB.Logs().Insert(log); err != nil {
		return errgo.Mask(err)
	}
	return nil
}

// StoreDatabase wraps an mgo.DB ands adds a few convenience methods.
type StoreDatabase struct {
	*mgo.Database
}

// Copy copies the StoreDatabase and its underlying mgo session.
func (s StoreDatabase) Copy() StoreDatabase {
	return StoreDatabase{
		&mgo.Database{
			Name:    s.Name,
			Session: s.Session.Copy(),
		},
	}
}

// Close closes the store database's underlying session.
func (s StoreDatabase) Close() {
	s.Session.Close()
}

// Entities returns the mongo collection where entities are stored.
func (s StoreDatabase) Entities() *mgo.Collection {
	return s.C("entities")
}

// BaseEntities returns the mongo collection where base entities are stored.
func (s StoreDatabase) BaseEntities() *mgo.Collection {
	return s.C("base_entities")
}

// Logs returns the Mongo collection where charm store logs are stored.
func (s StoreDatabase) Logs() *mgo.Collection {
	return s.C("logs")
}

// Migrations returns the Mongo collection where the migration info is stored.
func (s StoreDatabase) Migrations() *mgo.Collection {
	return s.C("migrations")
}

func (s StoreDatabase) Macaroons() *mgo.Collection {
	return s.C("macaroons")
}

// allCollections holds for each collection used by the charm store a
// function returns that collection.
var allCollections = []func(StoreDatabase) *mgo.Collection{
	StoreDatabase.StatCounters,
	StoreDatabase.StatTokens,
	StoreDatabase.Entities,
	StoreDatabase.BaseEntities,
	StoreDatabase.Logs,
	StoreDatabase.Migrations,
	StoreDatabase.Macaroons,
}

// Collections returns a slice of all the collections used
// by the charm store.
func (s StoreDatabase) Collections() []*mgo.Collection {
	cs := make([]*mgo.Collection, len(allCollections))
	for i, f := range allCollections {
		cs[i] = f(s)
	}
	return cs
}

type readerAtSeeker struct {
	r io.ReadSeeker
}

func (r *readerAtSeeker) ReadAt(buf []byte, p int64) (int, error) {
	if _, err := r.r.Seek(p, 0); err != nil {
		return 0, errgo.Notef(err, "cannot seek")
	}
	return r.r.Read(buf)
}

// ReaderAtSeeker adapts r so that it can be used as
// a ReaderAt. Note that, unlike some implementations
// of ReaderAt, it is not OK to use concurrently.
func ReaderAtSeeker(r io.ReadSeeker) io.ReaderAt {
	return &readerAtSeeker{r}
}

// Search searches the store for the given SearchParams.
// It returns a SearchResult containing the results of the search.
func (store *Store) Search(sp SearchParams) (SearchResult, error) {
	result, err := store.ES.search(sp)
	if err != nil {
		return SearchResult{}, errgo.Mask(err)
	}
	return result, nil
}

// SynchroniseElasticsearch creates new indexes in elasticsearch
// and populates them with the current data from the mongodb database.
func (s *Store) SynchroniseElasticsearch() error {
	if err := s.ES.ensureIndexes(true); err != nil {
		return errgo.Notef(err, "cannot create indexes")
	}
	if err := s.syncSearch(); err != nil {
		return errgo.Notef(err, "cannot synchronise indexes")
	}
	return nil
}
