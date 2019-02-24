package varastoserver

import (
	"bytes"
	"encoding/base64"
	"github.com/asdine/storm"
	"github.com/function61/eventkit/event"
	"github.com/function61/eventkit/eventlog"
	"github.com/function61/gokit/httpauth"
	"github.com/function61/gokit/logex"
	"github.com/function61/varasto/pkg/stateresolver"
	"github.com/function61/varasto/pkg/varastotypes"
	"github.com/gorilla/mux"
	"io"
	"io/ioutil"
	"net/http"
)

type handlers struct {
	db   *storm.DB
	conf *ServerConfig
}

func convertDir(dir varastotypes.Directory) Directory {
	return Directory{
		Id:     dir.ID,
		Parent: dir.Parent,
		Name:   dir.Name,
	}
}

func convertDbCollection(coll varastotypes.Collection, changesets []ChangesetSubset) CollectionSubset {
	return CollectionSubset{
		Id:                coll.ID,
		Directory:         coll.Directory,
		Name:              coll.Name,
		ReplicationPolicy: coll.ReplicationPolicy,
		Changesets:        changesets,
	}
}

func (h *handlers) GetDirectory(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *DirectoryOutput {
	dirId := mux.Vars(r)["id"]

	tx, err := h.db.Begin(false)
	panicIfError(err)
	defer tx.Rollback()

	dir, err := QueryWithTx(tx).Directory(dirId)
	panicIfError(err)

	parentDirs, err := getParentDirs(*dir, tx)
	panicIfError(err)

	parentDirsConverted := []Directory{}

	for _, parentDir := range parentDirs {
		parentDirsConverted = append(parentDirsConverted, convertDir(parentDir))
	}

	dbColls := []varastotypes.Collection{}
	if err := tx.Find("Directory", dir.ID, &dbColls); err != nil && err != storm.ErrNotFound {
		panic(err)
	}

	colls := []CollectionSubset{}
	for _, dbColl := range dbColls {
		colls = append(colls, convertDbCollection(dbColl, nil)) // FIXME: nil ok?
	}

	dbSubDirs := []varastotypes.Directory{}
	if err := tx.Find("Parent", dir.ID, &dbSubDirs); err != nil && err != storm.ErrNotFound {
		panic(err)
	}

	subDirs := []Directory{}
	for _, dbSubDir := range dbSubDirs {
		subDirs = append(subDirs, convertDir(dbSubDir))
	}

	return &DirectoryOutput{
		Directory:   convertDir(*dir),
		Parents:     parentDirsConverted,
		Directories: subDirs,
		Collections: colls,
	}
}

func (h *handlers) GetCollectiotAtRev(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *CollectionOutput {
	collectionId := mux.Vars(r)["id"]
	changesetId := mux.Vars(r)["rev"]
	pathBytes, err := base64.StdEncoding.DecodeString(mux.Vars(r)["path"])
	if err != nil {
		panic(err)
	}

	tx, err := h.db.Begin(false)
	panicIfError(err)
	defer tx.Rollback()

	coll, err := QueryWithTx(tx).Collection(collectionId)
	if err != nil {
		if err == ErrDbRecordNotFound {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return nil
	}

	if changesetId == HeadRevisionId {
		changesetId = coll.Head
	}

	state, err := stateresolver.ComputeStateAt(*coll, changesetId)
	panicIfError(err)

	allFilesInRevision := state.FileList()

	// peek brings a subset of allFilesInRevision
	peekResult := stateresolver.DirPeek(allFilesInRevision, string(pathBytes))

	totalSize := int64(0)
	convertedFiles := []File{}

	for _, file := range allFilesInRevision {
		totalSize += file.Size
	}

	for _, file := range peekResult.Files {
		convertedFiles = append(convertedFiles, File{
			Path:     file.Path,
			Sha256:   file.Sha256,
			Created:  file.Created,
			Modified: file.Modified,
			Size:     int(file.Size), // FIXME
			BlobRefs: file.BlobRefs,
		})
	}

	changesetsConverted := []ChangesetSubset{}

	for _, changeset := range coll.Changesets {
		changesetsConverted = append(changesetsConverted, ChangesetSubset{
			Id:      changeset.ID,
			Parent:  changeset.Parent,
			Created: changeset.Created,
		})
	}

	return &CollectionOutput{
		TotalSize: int(totalSize), // FIXME
		SelectedPathContents: SelectedPathContents{
			Path:       peekResult.Path,
			Files:      convertedFiles,
			ParentDirs: peekResult.ParentDirs,
			SubDirs:    peekResult.SubDirs,
		},
		FileCount:   len(allFilesInRevision),
		ChangesetId: changesetId,
		Collection:  convertDbCollection(*coll, changesetsConverted),
	}
}

// TODO: URL parameter comes via a hack in frontend
func (h *handlers) DownloadFile(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) {
	collectionId := mux.Vars(r)["id"]
	changesetId := mux.Vars(r)["rev"]

	fileKey := r.URL.Query().Get("file")

	tx, err := h.db.Begin(false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	coll, err := QueryWithTx(tx).Collection(collectionId)
	if err != nil {
		if err == ErrDbRecordNotFound {
			http.Error(w, "collection not found", http.StatusNotFound)
			return
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	state, err := stateresolver.ComputeStateAt(*coll, changesetId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	files := state.Files()
	file, found := files[fileKey]
	if !found {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	type RefAndVolumeId struct {
		Ref      varastotypes.BlobRef
		VolumeId int
	}

	refAndVolumeIds := []RefAndVolumeId{}
	for _, refSerialized := range file.BlobRefs {
		ref, err := varastotypes.BlobRefFromHex(refSerialized)
		if err != nil { // should not happen
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		blob, err := QueryWithTx(tx).Blob(*ref)
		if err != nil {
			if err == ErrDbRecordNotFound {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else {
				http.Error(w, "blob pointed to by file metadata not found", http.StatusInternalServerError)
				return
			}
		}

		volumeId, found := volumeManagerBestVolumeIdForBlob(blob.Volumes, h.conf)
		if !found {
			http.Error(w, varastotypes.ErrBlobNotAccessibleOnThisNode.Error(), http.StatusInternalServerError)
			return
		}

		refAndVolumeIds = append(refAndVolumeIds, RefAndVolumeId{
			Ref:      *ref,
			VolumeId: volumeId,
		})
	}

	h.db.Rollback() // eagerly b/c the below operation is slow

	w.Header().Set("Content-Type", contentTypeForFilename(fileKey))

	for _, refAndVolumeId := range refAndVolumeIds {
		chunkStream, err := h.conf.VolumeDrivers[refAndVolumeId.VolumeId].Fetch(
			refAndVolumeId.Ref)
		panicIfError(err)

		if _, err := io.Copy(w, chunkStream); err != nil {
			panic(err)
		}

		chunkStream.Close()
	}
}

func (h *handlers) GetVolumes(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *[]Volume {
	ret := []Volume{}

	dbObjects := []varastotypes.Volume{}
	panicIfError(h.db.All(&dbObjects))

	for _, dbObject := range dbObjects {
		ret = append(ret, Volume{
			Id:            dbObject.ID,
			Uuid:          dbObject.UUID,
			Label:         dbObject.Label,
			Quota:         int(dbObject.Quota), // FIXME: lossy conversions here
			BlobSizeTotal: int(dbObject.BlobSizeTotal),
			BlobCount:     int(dbObject.BlobCount),
		})
	}

	return &ret
}

func (h *handlers) GetVolumeMounts(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *[]VolumeMount {
	ret := []VolumeMount{}

	dbObjects := []varastotypes.VolumeMount{}
	panicIfError(h.db.All(&dbObjects))

	for _, dbObject := range dbObjects {
		ret = append(ret, VolumeMount{
			Id:         dbObject.ID,
			Volume:     dbObject.Volume,
			Node:       dbObject.Node,
			Driver:     string(dbObject.Driver), // FIXME: string enum to frontend
			DriverOpts: dbObject.DriverOpts,
		})
	}

	return &ret
}

func (h *handlers) GetReplicationPolicies(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *[]ReplicationPolicy {
	ret := []ReplicationPolicy{}

	dbObjects := []varastotypes.ReplicationPolicy{}
	panicIfError(h.db.All(&dbObjects))

	for _, dbObject := range dbObjects {
		ret = append(ret, ReplicationPolicy{
			Id:             dbObject.ID,
			Name:           dbObject.Name,
			DesiredVolumes: dbObject.DesiredVolumes,
		})
	}

	return &ret
}

func (h *handlers) GetNodes(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *[]Node {
	ret := []Node{}

	dbObjects := []varastotypes.Node{}
	panicIfError(h.db.All(&dbObjects))

	for _, dbObject := range dbObjects {
		ret = append(ret, Node{
			Id:   dbObject.ID,
			Addr: dbObject.Addr,
			Name: dbObject.Name,
		})
	}

	return &ret
}

func (h *handlers) GetClients(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) *[]Client {
	ret := []Client{}

	dbObjects := []varastotypes.Client{}
	panicIfError(h.db.All(&dbObjects))

	for _, dbObject := range dbObjects {
		ret = append(ret, Client{
			Id:        dbObject.ID,
			Name:      dbObject.Name,
			AuthToken: dbObject.AuthToken,
		})
	}

	return &ret
}

func (h *handlers) DatabaseExport(rctx *httpauth.RequestContext, w http.ResponseWriter, r *http.Request) {
	tx, err := h.db.Begin(false)
	panicIfError(err)
	defer tx.Rollback()

	panicIfError(exportDb(tx, w))
}

// func createNonPersistingEventLog(listeners domain.EventListener) (eventlog.Log, error) {
func createNonPersistingEventLog() (eventlog.Log, error) {
	return eventlog.NewSimpleLogFile(
		bytes.NewReader(nil),
		ioutil.Discard,
		func(e event.Event) error {
			return nil
			// return domain.DispatchEvent(e, listeners)
		},
		func(serialized string) (event.Event, error) {
			return nil, nil
			// return event.Deserialize(serialized, domain.Allocators)
		},
		logex.Discard)
}

func createDummyMiddlewares(conf *ServerConfig) httpauth.MiddlewareChainMap {
	return httpauth.MiddlewareChainMap{
		"public": func(w http.ResponseWriter, r *http.Request) *httpauth.RequestContext {
			return &httpauth.RequestContext{}
		},
		"authenticated": func(w http.ResponseWriter, r *http.Request) *httpauth.RequestContext {
			if !authenticate(conf, w, r) {
				return nil
			}

			return &httpauth.RequestContext{}
		},
	}
}

func getParentDirs(of varastotypes.Directory, tx storm.Node) ([]varastotypes.Directory, error) {
	parentDirs := []varastotypes.Directory{}

	current := &of
	var err error

	for current.Parent != "" {
		current, err = QueryWithTx(tx).Directory(current.Parent)
		if err != nil {
			return nil, err
		}

		// reverse order
		parentDirs = append([]varastotypes.Directory{*current}, parentDirs...)
	}

	return parentDirs, nil
}