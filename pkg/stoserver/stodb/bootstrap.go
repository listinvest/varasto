package stodb

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"github.com/function61/gokit/fileexists"
	"github.com/function61/gokit/logex"
	"github.com/function61/varasto/pkg/sslca"
	"github.com/function61/varasto/pkg/stoclient"
	"github.com/function61/varasto/pkg/stoserver/stoservertypes"
	"github.com/function61/varasto/pkg/stotypes"
	"github.com/function61/varasto/pkg/stoutils"
	"go.etcd.io/bbolt"
)

// opens BoltDB database
func Open(dbLocation string) (*bbolt.DB, error) {
	return bbolt.Open(dbLocation, 0700, nil)
}

func Bootstrap(db *bbolt.DB, logger *log.Logger) error {
	logl := logex.Levels(logger)

	bootstrapTimestamp := time.Now()

	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	defer func() { ignoreError(tx.Rollback()) }()

	// be extra safe and scan the DB to see that it is totally empty
	if err := tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
		return fmt.Errorf("DB not empty, found bucket: %s", name)
	}); err != nil {
		return err
	}

	if err := BootstrapRepos(tx); err != nil {
		return err
	}

	// it'd be dangerous to use os.Hostname() and assume it's resolvable from clients, so
	// better leave it to the user to configure it explicitly later
	hostname := "localhost"

	privKeyPem, err := sslca.GenEcP256PrivateKeyPem()
	if err != nil {
		return err
	}

	certPem, err := sslca.SelfSignedServerCert(hostname, "Varasto self-signed", privKeyPem)
	if err != nil {
		return err
	}

	// if we're not inside Docker, we need to use SMART via Docker image because its
	// automation friendly JSON interface is not in mainstream OSes yet
	smartBackend := stoservertypes.SmartBackendSmartCtlViaDocker
	if maybeRunningInsideDocker() {
		// when we're in Docker, we guess we're using the official Varasto image which
		// has the exact correct version of smartctl and so we can invoke it directly
		smartBackend = stoservertypes.SmartBackendSmartCtl
	}

	newNode := &stotypes.Node{
		ID:           stoutils.NewNodeId(),
		Addr:         "https://" + hostname,
		Name:         "Primary",
		TlsCert:      string(certPem),
		SmartBackend: smartBackend,
	}

	logl.Info.Printf("generated nodeId: %s", newNode.ID)

	systemAuthToken := stoutils.NewApiKeySecret()

	rootDir := stotypes.NewDirectory(
		"root",
		"",
		"root",
		string(stoservertypes.DirectoryTypeGeneric))
	rootDir.ReplicationPolicy = "default"

	results := []error{
		NodeRepository.Update(newNode, tx),
		DirectoryRepository.Update(rootDir, tx),
		VolumeRepository.Update(&stotypes.Volume{
			ID:         1,
			UUID:       stoutils.NewVolumeUuid(),
			Label:      "Default volume",
			Technology: string(stoservertypes.VolumeTechnologyDiskHdd),
			Quota:      1 * 1024 * 1024 * 1024,
			Zone:       "Default",
		}, tx),
		ReplicationPolicyRepository.Update(&stotypes.ReplicationPolicy{
			ID:             "default",
			Name:           "Default",
			DesiredVolumes: []int{1},
			MinZones:       1,
		}, tx),
		ClientRepository.Update(&stotypes.Client{
			ID:        stoutils.NewClientId(),
			Created:   bootstrapTimestamp,
			Name:      "System",
			AuthToken: systemAuthToken,
		}, tx),
		ScheduledJobRepository.Update(scheduledJobSeedSmartPoller(), tx),
		ScheduledJobRepository.Update(scheduledJobSeedMetadataBackup(), tx),
		ScheduledJobRepository.Update(ScheduledJobSeedVersionUpdateCheck(), tx),
		CfgNodeId.Set(newNode.ID, tx),
		CfgNodeTlsCertKey.Set(string(privKeyPem), tx),
	}

	if err := allOk(results); err != nil {
		return err
	}

	if err := configureClientConfig(systemAuthToken); err != nil {
		return err
	}

	return tx.Commit()
}

func BootstrapRepos(tx *bbolt.Tx) error {
	if err := writeSchemaVersionCurrent(tx); err != nil {
		return err
	}

	for _, repo := range RepoByRecordType {
		if err := repo.Bootstrap(tx); err != nil {
			return err
		}
	}

	return nil
}

func configureClientConfig(authToken string) error {
	conf := &stoclient.ClientConfig{
		ServerAddr:                "https://localhost",
		AuthToken:                 authToken,
		FuseMountPath:             "/mnt/varasto/stofuse/varasto",
		TlsInsecureSkipValidation: true, // localhost doesn't have MITM risk
	}

	confPath, err := stoclient.ConfigFilePath()
	if err != nil {
		return err
	}

	exists, err := fileexists.ExistsNoLinkFollow(confPath)
	if err != nil {
		return err
	}

	// config already exists? don't overwrite it..
	if exists {
		// .. but if it's a symlink to a non-existent config file then write the target.
		// at least in our Docker install we have symlink to a stateful volume.
		return writeConfigToDestinationIfSymlink(conf, confPath)
	}

	return stoclient.WriteConfig(conf)
}

func writeConfigToDestinationIfSymlink(conf *stoclient.ClientConfig, confPath string) error {
	linkDest, err := os.Readlink(confPath)
	if err != nil {
		return nil // maybe it wasn't a link (or some other error.. TODO: currently ignored)
	}

	linkDestExists, err := fileexists.Exists(linkDest)
	if err != nil {
		return err
	}

	if linkDestExists {
		return nil // don't overwrite
	}

	return stoclient.WriteConfigWithPath(conf, linkDest)
}

func allOk(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func ignoreError(err error) {
	// no-op
}

// if false, we might not be running in Docker (also any error)
// if true, we are most probably running in Docker
func maybeRunningInsideDocker() bool {
	// https://stackoverflow.com/a/20012536
	initCgroups, err := ioutil.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}

	return strings.Contains(string(initCgroups), "docker")
}
