package stoserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/function61/eventkit/command"
	"github.com/function61/eventkit/eventlog"
	"github.com/function61/eventkit/httpcommand"
	"github.com/function61/gokit/cryptoutil"
	"github.com/function61/gokit/httpauth"
	"github.com/function61/gokit/logex"
	"github.com/function61/gokit/sliceutil"
	"github.com/function61/varasto/pkg/blorm"
	"github.com/function61/varasto/pkg/smart"
	"github.com/function61/varasto/pkg/stofuse/stofuseclient"
	"github.com/function61/varasto/pkg/stoserver/stodb"
	"github.com/function61/varasto/pkg/stoserver/stointegrityverifier"
	"github.com/function61/varasto/pkg/stoserver/stoservertypes"
	"github.com/function61/varasto/pkg/stotypes"
	"github.com/function61/varasto/pkg/stoutils"
	"github.com/gorilla/mux"
	"go.etcd.io/bbolt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// we are currently using the command pattern very wrong!
type cHandlers struct {
	db           *bolt.DB
	conf         *ServerConfig
	ivController *stointegrityverifier.Controller
	logger       *log.Logger
}

func (c *cHandlers) VolumeCreate(cmd *stoservertypes.VolumeCreate, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		max := 0

		allVolumes := []stotypes.Volume{}
		if err := stodb.VolumeRepository.Each(stodb.VolumeAppender(&allVolumes), tx); err != nil {
			return err
		}

		for _, vol := range allVolumes {
			if vol.ID > max {
				max = vol.ID
			}
		}

		return stodb.VolumeRepository.Update(&stotypes.Volume{
			ID:         max + 1,
			UUID:       stoutils.NewVolumeUuid(),
			Label:      cmd.Name,
			Technology: string(stoservertypes.VolumeTechnologyDiskHdd),
			Quota:      mebibytesToBytes(cmd.Quota),
		}, tx)
	})
}

func (c *cHandlers) SubsystemStart(cmd *stoservertypes.SubsystemStart, ctx *command.Ctx) error {
	subsys := c.getSubsystem(cmd.Id)
	if subsys == nil {
		panic("shouldnt happen")
	}

	if subsys.enabled {
		return fmt.Errorf("subsystem %s already enabled", cmd.Id)
	}
	subsys.enabled = !subsys.enabled

	subsys.controller.Start()
	return nil
}

func (c *cHandlers) SubsystemStop(cmd *stoservertypes.SubsystemStop, ctx *command.Ctx) error {
	subsys := c.getSubsystem(cmd.Id)
	if subsys == nil {
		panic("shouldnt happen")
	}

	if !subsys.enabled {
		return fmt.Errorf("subsystem %s already disabled", cmd.Id)
	}
	subsys.enabled = !subsys.enabled

	subsys.controller.Stop()
	return nil
}

func (c *cHandlers) VolumeChangeQuota(cmd *stoservertypes.VolumeChangeQuota, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.Quota = mebibytesToBytes(cmd.Quota)

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) VolumeSetManufacturingDate(cmd *stoservertypes.VolumeSetManufacturingDate, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.Manufactured = cmd.ManufacturingDate.Time

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) VolumeSetWarrantyEndDate(cmd *stoservertypes.VolumeSetWarrantyEndDate, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.WarrantyEnds = cmd.WarrantyEndDate.Time

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) VolumeSetSerialNumber(cmd *stoservertypes.VolumeSetSerialNumber, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.SerialNumber = cmd.SerialNumber

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) VolumeSetTechnology(cmd *stoservertypes.VolumeSetTechnology, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.Technology = string(cmd.Technology)

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) VolumeSetTopology(cmd *stoservertypes.VolumeSetTopology, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		if cmd.Slot == 0 && cmd.Enclosure != "" {
			return errors.New("Slot cannot be 0 when enclosure is defined")
		}

		vol.Enclosure = cmd.Enclosure
		vol.EnclosureSlot = cmd.Slot

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) VolumeChangeDescription(cmd *stoservertypes.VolumeChangeDescription, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.Description = cmd.Description

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

// FIXME: name ends in 2 because conflicts with types.VolumeMount
func (c *cHandlers) VolumeMount2(cmd *stoservertypes.VolumeMount2, ctx *command.Ctx) error {
	sameVolumeOnSameNode := func(a, b stotypes.VolumeMount) bool {
		return a.Volume == b.Volume && a.Node == b.Node
	}

	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		mountSpec := &stotypes.VolumeMount{
			ID:         stoutils.NewVolumeMountId(),
			Volume:     vol.ID,
			Node:       c.conf.SelfNodeId,
			Driver:     stotypes.VolumeDriverKind(cmd.Kind),
			DriverOpts: cmd.DriverOpts,
		}

		allMounts := []stotypes.VolumeMount{}
		if err := stodb.VolumeMountRepository.Each(stodb.VolumeMountAppender(&allMounts), tx); err != nil {
			return err
		}

		for _, otherMount := range allMounts {
			if sameVolumeOnSameNode(*mountSpec, otherMount) {
				return fmt.Errorf("same volume is already mounted at specified node. mount id: %s", otherMount.ID)
			}
		}

		// try mounting the volume
		driver, err := getDriver(*vol, *mountSpec, logex.Discard)
		if err != nil {
			return err
		}

		if err := driver.Mountable(context.TODO()); err != nil {
			return err
		}

		return stodb.VolumeMountRepository.Update(mountSpec, tx)
	})
}

func (c *cHandlers) VolumeUnmount(cmd *stoservertypes.VolumeUnmount, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		mount, err := stodb.Read(tx).VolumeMount(cmd.Id)
		if err != nil {
			return err
		}

		return stodb.VolumeMountRepository.Delete(mount, tx)
	})
}

// "copy any blobs that were on this volume, to another volume"
func (c *cHandlers) VolumeMigrateData(cmd *stoservertypes.VolumeMigrateData, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		from, err := stodb.Read(tx).Volume(cmd.From)
		if err != nil {
			return err
		}

		to, err := stodb.Read(tx).Volume(cmd.To)
		if err != nil {
			return err
		}

		return stodb.BlobRepository.Each(func(record interface{}) error {
			blob := record.(*stotypes.Blob)

			if !sliceutil.ContainsInt(blob.Volumes, from.ID) { // doesn't fit our criteria
				return nil
			}

			if sliceutil.ContainsInt(blob.Volumes, to.ID) { // is already in target volume
				return nil
			}

			blob.VolumesPendingReplication = append(blob.VolumesPendingReplication, to.ID)

			return stodb.BlobRepository.Update(blob, tx)
		}, tx)
	})
}

func (c *cHandlers) VolumeVerifyIntegrity(cmd *stoservertypes.VolumeVerifyIntegrity, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		job := &stotypes.IntegrityVerificationJob{
			ID:       stoutils.NewIntegrityVerificationJobId(),
			Started:  ctx.Meta.Timestamp,
			VolumeId: cmd.Id,
		}

		return stodb.IntegrityVerificationJobRepository.Update(job, tx)
	})
}

func (c *cHandlers) DirectoryCreate(cmd *stoservertypes.DirectoryCreate, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := validateUniqueNameWithinSiblings(cmd.Parent, cmd.Name, tx); err != nil {
			return err
		}

		return stodb.DirectoryRepository.Update(
			stotypes.NewDirectory(
				stoutils.NewDirectoryId(),
				cmd.Parent,
				cmd.Name,
				string(stoservertypes.DirectoryTypeGeneric)),
			tx)
	})
}

func (c *cHandlers) DirectoryDelete(cmd *stoservertypes.DirectoryDelete, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		dir, err := stodb.Read(tx).Directory(cmd.Id)
		if err != nil {
			return err
		}

		collections, err := stodb.Read(tx).CollectionsByDirectory(dir.ID)
		if err != nil {
			return err
		}

		subDirs, err := stodb.Read(tx).SubDirectories(dir.ID)
		if err != nil {
			return err
		}

		if len(collections) > 0 {
			return fmt.Errorf("Cannot delete directory because it has %d collection(s)", len(collections))
		}

		if len(subDirs) > 0 {
			return fmt.Errorf("Cannot delete directory because it has %d directory(s)", len(subDirs))
		}

		return stodb.DirectoryRepository.Delete(dir, tx)
	})
}

func (c *cHandlers) DirectoryRename(cmd *stoservertypes.DirectoryRename, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		dir, err := stodb.Read(tx).Directory(cmd.Id)
		if err != nil {
			return err
		}

		dir.Name = cmd.Name

		if err := validateUniqueNameWithinSiblings(dir.Parent, dir.Name, tx); err != nil {
			return err
		}

		return stodb.DirectoryRepository.Update(dir, tx)
	})
}

func (c *cHandlers) DirectoryChangeDescription(cmd *stoservertypes.DirectoryChangeDescription, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		dir, err := stodb.Read(tx).Directory(cmd.Id)
		if err != nil {
			return err
		}

		dir.Description = cmd.Description

		return stodb.DirectoryRepository.Update(dir, tx)
	})
}

func (c *cHandlers) DirectorySetType(cmd *stoservertypes.DirectorySetType, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		dir, err := stodb.Read(tx).Directory(cmd.Id)
		if err != nil {
			return err
		}

		dir.Type = string(cmd.Type)

		return stodb.DirectoryRepository.Update(dir, tx)
	})
}

func (c *cHandlers) DirectoryChangeSensitivity(cmd *stoservertypes.DirectoryChangeSensitivity, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := validateSensitivity(cmd.Sensitivity); err != nil {
			return err
		}

		dir, err := stodb.Read(tx).Directory(cmd.Id)
		if err != nil {
			return err
		}

		dir.Sensitivity = cmd.Sensitivity

		return stodb.DirectoryRepository.Update(dir, tx)
	})
}

func (c *cHandlers) DirectoryMove(cmd *stoservertypes.DirectoryMove, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		dirToMove, err := stodb.Read(tx).Directory(cmd.Id)
		if err != nil {
			return err
		}

		// verify that new parent exists
		newParent, err := stodb.Read(tx).Directory(cmd.Directory)
		if err != nil {
			return err
		}

		if dirToMove.ID == newParent.ID {
			return errors.New("dir cannot be its own parent, dawg")
		}

		dirToMove.Parent = newParent.ID

		if err := validateUniqueNameWithinSiblings(dirToMove.Parent, dirToMove.Name, tx); err != nil {
			return err
		}

		return stodb.DirectoryRepository.Update(dirToMove, tx)
	})
}

func (c *cHandlers) CollectionCreate(cmd *stoservertypes.CollectionCreate, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		if _, err := stodb.Read(tx).Directory(cmd.ParentDir); err != nil {
			if err == blorm.ErrNotFound {
				return errors.New("parent directory not found")
			} else {
				return err
			}
		}

		if err := validateUniqueNameWithinSiblings(cmd.ParentDir, cmd.Name, tx); err != nil {
			return err
		}

		// TODO: resolve this from closest parent that has policy defined?
		replicationPolicy, err := stodb.Read(tx).ReplicationPolicy("default")
		if err != nil {
			return err
		}

		if len(replicationPolicy.DesiredVolumes) == 0 {
			return errors.New("replicationPolicy doesn't specify any volumes")
		}

		kekPublicKeys := []rsa.PublicKey{}

		keks := []stotypes.KeyEncryptionKey{}
		if err := stodb.KeyEncryptionKeyRepository.Each(stodb.KeyEncryptionKeyAppender(&keks), tx); err != nil {
			return err
		}

		for _, kek := range keks {
			pubKey, err := cryptoutil.ParsePemPkcs1EncodedRsaPublicKey(strings.NewReader(kek.PublicKey))
			if err != nil {
				return err
			}

			kekPublicKeys = append(kekPublicKeys, *pubKey)
		}

		if len(kekPublicKeys) == 0 {
			return fmt.Errorf("no public keys found for encrypting %s", cmd.Name)
		}

		encryptionKey := [32]byte{}
		if _, err := rand.Read(encryptionKey[:]); err != nil {
			return err
		}

		// pack encryption key in an envelope protected with public key crypto,
		// so Varasto can store data without being able to access it itself
		encryptionKeyEnveloped, err := stotypes.EncryptEnvelope(stoutils.NewEncryptionKeyId(), encryptionKey[:], kekPublicKeys)
		if err != nil {
			return err
		}

		collection := &stotypes.Collection{
			ID:             stoutils.NewCollectionId(),
			Created:        time.Now(),
			Directory:      cmd.ParentDir,
			Name:           cmd.Name,
			DesiredVolumes: replicationPolicy.DesiredVolumes,
			Head:           stotypes.NoParentId,
			EncryptionKeys: []stotypes.KeyEnvelope{*encryptionKeyEnveloped},
			Changesets:     []stotypes.CollectionChangeset{},
			Metadata:       map[string]string{},
			Tags:           []string{},
		}

		// highly unlikely
		if _, err := stodb.Read(tx).Collection(collection.ID); err != blorm.ErrNotFound {
			return errors.New("accidentally generated duplicate collection ID")
		}

		ctx.CreatedRecordId(collection.ID)

		return stodb.CollectionRepository.Update(collection, tx)
	})
}

func (c *cHandlers) CollectionChangeSensitivity(cmd *stoservertypes.CollectionChangeSensitivity, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := validateSensitivity(cmd.Sensitivity); err != nil {
			return err
		}

		coll, err := stodb.Read(tx).Collection(cmd.Id)
		if err != nil {
			return err
		}

		coll.Sensitivity = cmd.Sensitivity

		return stodb.CollectionRepository.Update(coll, tx)
	})
}

func (c *cHandlers) CollectionMove(cmd *stoservertypes.CollectionMove, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		// check for existence
		if _, err := stodb.Read(tx).Directory(cmd.Directory); err != nil {
			return err
		}

		// Collection is validated as non-empty
		collIds := strings.Split(cmd.Collection, ",")

		for _, collId := range collIds {
			coll, err := stodb.Read(tx).Collection(collId)
			if err != nil {
				return err
			}

			if err := validateUniqueNameWithinSiblings(cmd.Directory, coll.Name, tx); err != nil {
				return err
			}

			coll.Directory = cmd.Directory

			if err := stodb.CollectionRepository.Update(coll, tx); err != nil {
				return err
			}
		}

		return nil
	})
}

func (c *cHandlers) CollectionChangeDescription(cmd *stoservertypes.CollectionChangeDescription, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		coll, err := stodb.Read(tx).Collection(cmd.Collection)
		if err != nil {
			return err
		}

		coll.Description = cmd.Description

		return stodb.CollectionRepository.Update(coll, tx)
	})
}

func (c *cHandlers) CollectionRename(cmd *stoservertypes.CollectionRename, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		coll, err := stodb.Read(tx).Collection(cmd.Collection)
		if err != nil {
			return err
		}

		coll.Name = cmd.Name

		if err := validateUniqueNameWithinSiblings(coll.Directory, coll.Name, tx); err != nil {
			return err
		}

		return stodb.CollectionRepository.Update(coll, tx)
	})
}

func (c *cHandlers) CollectionTag(cmd *stoservertypes.CollectionTag, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		coll, err := stodb.Read(tx).Collection(cmd.Id)
		if err != nil {
			return err
		}

		if sliceutil.ContainsString(coll.Tags, cmd.Tag) {
			return fmt.Errorf("already tagged: %s", cmd.Tag)
		}

		coll.Tags = append(coll.Tags, cmd.Tag)

		sort.Strings(coll.Tags)

		return stodb.CollectionRepository.Update(coll, tx)
	})
}

func (c *cHandlers) CollectionUntag(cmd *stoservertypes.CollectionUntag, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		coll, err := stodb.Read(tx).Collection(cmd.Id)
		if err != nil {
			return err
		}

		if !sliceutil.ContainsString(coll.Tags, cmd.Tag) {
			return fmt.Errorf("not tagged: %s", cmd.Tag)
		}

		coll.Tags = sliceutil.FilterString(coll.Tags, func(tag string) bool { return tag != cmd.Tag })

		return stodb.CollectionRepository.Update(coll, tx)
	})
}

func (c *cHandlers) CollectionFuseMount(cmd *stoservertypes.CollectionFuseMount, ctx *command.Ctx) error {
	tx, err := c.db.Begin(false)
	if err != nil {
		return err
	}
	defer func() { ignoreError(tx.Rollback()) }()

	baseUrl, err := stodb.CfgFuseServerBaseUrl.GetRequired(tx)
	if err != nil {
		return err
	}

	vstofuse := stofuseclient.New(baseUrl)

	if cmd.UnmountOthers {
		if err := vstofuse.UnmountAll(context.TODO()); err != nil {
			return err
		}
	}

	return vstofuse.Mount(context.TODO(), cmd.Collection)
}

func (c *cHandlers) CollectionDelete(cmd *stoservertypes.CollectionDelete, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		coll, err := stodb.Read(tx).Collection(cmd.Collection)
		if err != nil {
			return err
		}

		if cmd.Name != coll.Name {
			return fmt.Errorf("repeated name incorrect, expecting %s", coll.Name)
		}

		return stodb.CollectionRepository.Delete(coll, tx)
	})
}

func (c *cHandlers) ApikeyCreate(cmd *stoservertypes.ApikeyCreate, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return stodb.ClientRepository.Update(&stotypes.Client{
			ID:        stoutils.NewClientId(),
			Created:   ctx.Meta.Timestamp,
			Name:      cmd.Name,
			AuthToken: stoutils.NewApiKeyTokenId(),
		}, tx)
	})
}

func (c *cHandlers) ApikeyRemove(cmd *stoservertypes.ApikeyRemove, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return stodb.ClientRepository.Delete(&stotypes.Client{
			ID: cmd.Id,
		}, tx)
	})
}

func (c *cHandlers) IntegrityverificationjobResume(cmd *stoservertypes.IntegrityverificationjobResume, ctx *command.Ctx) error {
	c.ivController.Resume(cmd.JobId)

	return nil
}

func (c *cHandlers) IntegrityverificationjobStop(cmd *stoservertypes.IntegrityverificationjobStop, ctx *command.Ctx) error {
	c.ivController.Stop(cmd.JobId)

	return nil
}

func (c *cHandlers) ReplicationpolicyChangeDesiredVolumes(cmd *stoservertypes.ReplicationpolicyChangeDesiredVolumes, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		desiredVolumes := []int{}
		if err := json.Unmarshal([]byte(cmd.DesiredVolumes), &desiredVolumes); err != nil {
			return err
		}

		// verify that each volume exists
		for _, desiredVolume := range desiredVolumes {
			if _, err := stodb.Read(tx).Volume(desiredVolume); err != nil {
				return fmt.Errorf("desiredVolume %d: %v", desiredVolume, err)
			}
		}

		policy, err := stodb.Read(tx).ReplicationPolicy(cmd.Id)
		if err != nil {
			return err
		}

		policy.DesiredVolumes = desiredVolumes

		return stodb.ReplicationPolicyRepository.Update(policy, tx)
	})
}

func (c *cHandlers) ConfigSetFuseServerBaseurl(cmd *stoservertypes.ConfigSetFuseServerBaseurl, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return stodb.CfgFuseServerBaseUrl.Set(cmd.Baseurl, tx)
	})
}

func (c *cHandlers) ConfigSetNetworkShareBaseUrl(cmd *stoservertypes.ConfigSetNetworkShareBaseUrl, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return stodb.CfgNetworkShareBaseUrl.Set(cmd.Baseurl, tx)
	})
}

func (c *cHandlers) VolumeSmartSetId(cmd *stoservertypes.VolumeSmartSetId, ctx *command.Ctx) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		vol, err := stodb.Read(tx).Volume(cmd.Id)
		if err != nil {
			return err
		}

		vol.SmartId = cmd.SmartId

		return stodb.VolumeRepository.Update(vol, tx)
	})
}

func (c *cHandlers) getSubsystem(id stoservertypes.SubsystemId) *subsystem {
	switch stoservertypes.SubsystemIdExhaustived3ed3e(id) {
	case stoservertypes.SubsystemIdThumbnailGenerator:
		return c.conf.ThumbServer
	case stoservertypes.SubsystemIdFuseProjector:
		return c.conf.FuseProjector
	default:
		return nil
	}
}

func (c *cHandlers) NodeSmartScan(cmd *stoservertypes.NodeSmartScan, ctx *command.Ctx) error {
	type smartCapableVolume struct {
		volId   int
		smartId string
		report  *stoservertypes.SmartReport
	}

	scans := []*smartCapableVolume{}

	// list volumes that are capable of their SMART scan (for example cloud volumes obviously are not)
	if err := c.db.View(func(tx *bolt.Tx) error {
		return stodb.VolumeRepository.Each(func(record interface{}) error {
			vol := record.(*stotypes.Volume)

			if vol.SmartId != "" {
				scans = append(scans, &smartCapableVolume{
					volId:   vol.ID,
					smartId: vol.SmartId,
				})
			}

			return nil
		}, tx)
	}); err != nil {
		return err
	}

	for _, scan := range scans {
		report, err := smart.Scan(scan.smartId, smart.SmartCtlDockerBackend)
		if err != nil {
			return fmt.Errorf("volume %d (%s) error scanning SMART: %v", scan.volId, scan.smartId, err)
		}

		var temp *int
		var powerOnTime *int
		var powerCycleCount *int

		if report.Temperature.Current != 0 {
			temp = &report.Temperature.Current
		}

		if report.PowerOnTime.Hours != 0 {
			powerOnTime = &report.PowerOnTime.Hours
		}

		if report.PowerCycleCount != 0 {
			powerCycleCount = &report.PowerCycleCount
		}

		scan.report = &stoservertypes.SmartReport{
			Time:            time.Now(),
			Passed:          report.SmartStatus.Passed,
			Temperature:     temp,
			PowerCycleCount: powerCycleCount,
			PowerOnTime:     powerOnTime,
		}
	}

	// nothing to do
	if len(scans) == 0 {
		return nil
	}

	return c.db.Update(func(tx *bolt.Tx) error {
		for _, scan := range scans {
			vol, err := stodb.Read(tx).Volume(scan.volId)
			if err != nil {
				return err
			}

			// update back into db
			smartReportJson, err := json.Marshal(scan.report)
			if err != nil {
				return err
			}

			vol.SmartReport = string(smartReportJson)

			if err := stodb.VolumeRepository.Update(vol, tx); err != nil {
				return err
			}
		}

		return nil
	})
}

func registerCommandEndpoints(
	router *mux.Router,
	eventLog eventlog.Log,
	cmdHandlers stoservertypes.CommandHandlers,
	mwares httpauth.MiddlewareChainMap,
) {
	router.HandleFunc("/command/{commandName}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commandName := mux.Vars(r)["commandName"]

		httpErr := httpcommand.Serve(
			w,
			r,
			mwares,
			commandName,
			stoservertypes.Allocators,
			cmdHandlers,
			eventLog)
		if httpErr != nil {
			if !httpErr.ErrorResponseAlreadySentByMiddleware() {
				http.Error(
					w,
					httpErr.ErrorCode+": "+httpErr.Description,
					httpErr.StatusCode) // making many assumptions here
			}
		} else {
			// no-op => ok
			_, _ = w.Write([]byte(`{}`))
		}
	})).Methods(http.MethodPost)
}

func mebibytesToBytes(mebibytes int) int64 {
	return int64(mebibytes * 1024 * 1024)
}

func validateSensitivity(in int) error {
	if in < 0 || in > 2 {
		return fmt.Errorf("sensitivity needs to be between 0-2; was %d", in)
	}

	return nil
}

// conflict could arise, when directory OR collection:
// - is created as a sibling with non-unique name
// - is renamed to non-unique name
// - once unique-within-siblings item is moved into a directory where name already exists
func validateUniqueNameWithinSiblings(dirId string, name string, tx *bolt.Tx) error {
	siblingDirectories, err := stodb.Read(tx).SubDirectories(dirId)
	if err != nil {
		return err
	}

	siblingCollections, err := stodb.Read(tx).CollectionsByDirectory(dirId)
	if err != nil {
		return err
	}

	for _, siblingDirectory := range siblingDirectories {
		if siblingDirectory.Name == name {
			return fmt.Errorf("directory %s already exists as a sibling", name)
		}
	}

	for _, siblingCollection := range siblingCollections {
		if siblingCollection.Name == name {
			return fmt.Errorf("collection %s already exists as a sibling", name)
		}
	}

	return nil
}
