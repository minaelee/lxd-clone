package drivers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/canonical/lxd/lxd/archive"
	"github.com/canonical/lxd/lxd/backup"
	"github.com/canonical/lxd/lxd/instance/instancetype"
	"github.com/canonical/lxd/lxd/instancewriter"
	"github.com/canonical/lxd/lxd/migration"
	"github.com/canonical/lxd/lxd/operations"
	"github.com/canonical/lxd/lxd/storage/block"
	"github.com/canonical/lxd/lxd/storage/filesystem"
	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/api"
	"github.com/canonical/lxd/shared/ioprogress"
	"github.com/canonical/lxd/shared/logger"
	"github.com/canonical/lxd/shared/revert"
	"github.com/canonical/lxd/shared/units"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied filler function.
func (d *btrfs) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	volPath := vol.MountPath()

	// Setup revert.
	revert := revert.New()
	defer revert.Fail()

	// Create the volume itself.
	_, err := shared.RunCommandContext(context.TODO(), "btrfs", "subvolume", "create", volPath)
	if err != nil {
		return err
	}

	revert.Add(func() {
		_ = d.deleteSubvolume(volPath, false)
		_ = os.Remove(volPath)
	})

	// Create sparse loopback file if volume is block.
	rootBlockPath := ""
	if IsContentBlock(vol.contentType) {
		// We expect the filler to copy the VM image into this path.
		rootBlockPath, err = d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}

		// Get underlying btrfs mount options.
		mountinfo, err := filesystem.GetMountinfo(volPath)
		if err != nil {
			return err
		}

		mountOptions := strings.Split(d.getMountOptions(), ",")

		// Enable nodatacow on the parent directory so that when the root disk file is created the setting
		// is inherited and random writes don't cause fragmentation and old extents to be kept.
		// BTRFS extents are immutable so when blocks are written they end up in new extents and the old
		// ones remains until all of its data is dereferenced or rewritten. These old extents are counted
		// in the quota, and so leaving CoW enabled can cause the BTRFS subvolume quota to be reached even
		// before the block file itself is full. This setting does not totally prevents CoW from happening
		// as when a snapshot is taken, writes that happen on the original volume necessarily create a CoW
		// in order to track the difference between original and snapshot. This will increase the size of
		// data being referenced.
		//
		// An exception is made for when compression is enabled on the underlying storage.
		if !slices.Contains(mountOptions, "datacow") && !strings.Contains(mountinfo[len(mountinfo)-1], "compress") {
			_, err = shared.RunCommandContext(context.TODO(), "chattr", "+C", volPath)
			if err != nil {
				return fmt.Errorf("Failed setting nodatacow on %q: %w", volPath, err)
			}
		}
	}

	err = d.runFiller(vol, rootBlockPath, filler, false)
	if err != nil {
		return err
	}

	// If we are creating a block volume, resize it to the requested size or the default.
	// We expect the filler function to have converted the qcow2 image to raw into the rootBlockPath.
	if IsContentBlock(vol.contentType) {
		// Convert to bytes.
		sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
		if err != nil {
			return err
		}

		// Normally we pass VolumeTypeImage to unsupportedResizeTypes to ensureVolumeBlockFile as block
		// image volumes cannot be resized because they have a readonly snapshot which the instances are
		// created from, and that doesn't get updated when the original volumes size is changed.
		// However during initial volume fill we allow growing of image volumes because the snapshot hasn't
		// been taken yet. This is why no unsupported volume types are passed to ensureVolumeBlockFile.
		// This is important, as combined with not enabling allowUnsafeResize it still prevents us from
		// accidentally shrinking the filled volume if it is larger than vol.ConfigSize().
		// In that situation ensureVolumeBlockFile returns ErrCannotBeShrunk, but we ignore it as this just
		// means the filler run above has needed to increase the volume size beyond the default block
		// volume size.
		_, err = ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, false)
		if err != nil && !errors.Is(err, ErrCannotBeShrunk) {
			return err
		}

		// Move the GPT alt header to end of disk if needed and if filler specified.
		if vol.IsVMBlock() && filler != nil && filler.Fill != nil {
			err = d.moveGPTAltHeader(rootBlockPath)
			if err != nil {
				return err
			}
		}
	} else if vol.contentType == ContentTypeFS {
		// Set initial quota for filesystem volumes.
		err := d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		if err != nil {
			return err
		}
	}

	// Tweak any permissions that need tweaking after filling.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	// Attempt to mark image read-only.
	if vol.volType == VolumeTypeImage {
		err = d.setSubvolumeReadonlyProperty(volPath, true)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// CreateVolumeFromBackup restores a backup tarball onto the storage device.
func (d *btrfs) CreateVolumeFromBackup(vol VolumeCopy, srcBackup backup.Info, srcData io.ReadSeeker, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	// Handle the non-optimized tarballs through the generic unpacker.
	if !*srcBackup.OptimizedStorage {
		return genericVFSBackupUnpack(d, d.state, vol, srcBackup.Snapshots, srcData, op)
	}

	volExists, err := d.HasVolume(vol.Volume)
	if err != nil {
		return nil, nil, err
	}

	if volExists {
		return nil, nil, errors.New("Cannot restore volume, already exists on target")
	}

	revert := revert.New()
	defer revert.Fail()

	// Define a revert function that will be used both to revert if an error occurs inside this
	// function but also return it for use from the calling functions if no error internally.
	revertHook := func() {
		for _, snapName := range srcBackup.Snapshots {
			fullSnapshotName := GetSnapshotVolumeName(vol.name, snapName)
			snapVol := NewVolume(d, d.name, vol.volType, vol.contentType, fullSnapshotName, vol.config, vol.poolConfig)
			_ = d.DeleteVolumeSnapshot(snapVol, op)
		}

		// And lastly the main volume.
		_ = d.DeleteVolume(vol.Volume, op)
	}
	// Only execute the revert function if we have had an error internally.
	revert.Add(revertHook)

	// Find the compression algorithm used for backup source data.
	_, err = srcData.Seek(0, io.SeekStart)
	if err != nil {
		return nil, nil, err
	}

	_, _, unpacker, err := shared.DetectCompressionFile(srcData)
	if err != nil {
		return nil, nil, err
	}

	// Load optimized backup header file if specified.
	var optimizedHeader *BTRFSMetaDataHeader
	if *srcBackup.OptimizedHeader {
		optimizedHeader, err = d.loadOptimizedBackupHeader(srcData, GetVolumeMountPath(d.name, vol.volType, ""))
		if err != nil {
			return nil, nil, err
		}
	}

	// Populate optimized header with pseudo data for unified handling when backup doesn't contain the
	// optimized header file. This approach can only be used to restore root subvolumes (not sub-subvolumes).
	if optimizedHeader == nil {
		optimizedHeader = &BTRFSMetaDataHeader{}
		for _, snapName := range srcBackup.Snapshots {
			optimizedHeader.Subvolumes = append(optimizedHeader.Subvolumes, BTRFSSubVolume{
				Snapshot: snapName,
				Path:     string(filepath.Separator),
				Readonly: true, // Snapshots are made readonly.
			})
		}

		optimizedHeader.Subvolumes = append(optimizedHeader.Subvolumes, BTRFSSubVolume{
			Snapshot: "",
			Path:     string(filepath.Separator),
			Readonly: false,
		})
	}

	// Create a temporary directory to unpack the backup into.
	tmpUnpackDir, err := os.MkdirTemp(GetVolumeMountPath(d.name, vol.volType, ""), "backup.")
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to create temporary directory %q: %w", tmpUnpackDir, err)
	}

	defer func() { _ = os.RemoveAll(tmpUnpackDir) }()

	err = os.Chmod(tmpUnpackDir, 0100)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to chmod temporary directory %q: %w", tmpUnpackDir, err)
	}

	// unpackSubVolume unpacks a subvolume file from a backup tarball file.
	unpackSubVolume := func(r io.ReadSeeker, unpacker []string, srcFile string, targetPath string) (string, error) {
		tr, cancelFunc, err := archive.CompressedTarReader(d.state, context.Background(), r, unpacker, targetPath)
		if err != nil {
			return "", err
		}

		defer cancelFunc()

		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break // End of archive.
			}

			if err != nil {
				return "", err
			}

			if hdr.Name == srcFile {
				subVolRecvPath, err := d.receiveSubVolume(tr, targetPath, nil)
				if err != nil {
					return "", err
				}

				cancelFunc()
				return subVolRecvPath, nil
			}
		}

		return "", fmt.Errorf("Could not find %q", srcFile)
	}

	type btrfsCopyOp struct {
		src  string
		dest string
	}

	var copyOps []btrfsCopyOp

	// unpackVolume unpacks all subvolumes in a LXD volume from a backup tarball file.
	unpackVolume := func(v Volume, srcFilePrefix string) error {
		_, snapName, _ := api.GetParentAndSnapshotName(v.name)

		for _, subVol := range optimizedHeader.Subvolumes {
			if subVol.Snapshot != snapName {
				continue // Skip any subvolumes that dont belong to our volume (empty for main).
			}

			// Figure out what file we are looking for in the backup file.
			srcFilePath := filepath.Join("backup", srcFilePrefix+".bin")
			if subVol.Path != string(filepath.Separator) {
				// If subvolume is non-root, then we expect the file to be encoded as its original
				// path with the leading / removed.
				srcFilePath = filepath.Join("backup", srcFilePrefix+"_"+filesystem.PathNameEncode(strings.TrimPrefix(subVol.Path, string(filepath.Separator)))+".bin")
			}

			// Define where we will move the subvolume after it is unpacked.
			subVolTargetPath := filepath.Join(v.MountPath(), subVol.Path)

			tmpUnpackDir := filepath.Join(tmpUnpackDir, snapName)

			err := os.MkdirAll(tmpUnpackDir, 0100)
			if err != nil {
				return fmt.Errorf("Failed creating directory %q: %w", tmpUnpackDir, err)
			}

			d.Logger().Debug("Unpacking optimized volume", logger.Ctx{"name": v.name, "source": srcFilePath, "unpackPath": tmpUnpackDir, "path": subVolTargetPath})

			// Unpack the volume into the temporary unpackDir.
			unpackedSubVolPath, err := unpackSubVolume(srcData, unpacker, srcFilePath, tmpUnpackDir)
			if err != nil {
				return err
			}

			copyOps = append(copyOps, btrfsCopyOp{
				src:  unpackedSubVolPath,
				dest: subVolTargetPath,
			})
		}

		return nil
	}

	if len(srcBackup.Snapshots) > 0 {
		// Create new snapshots directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return nil, nil, err
		}

		// Restore backup snapshots from oldest to newest.
		for _, snapName := range srcBackup.Snapshots {
			// Defend against path traversal attacks.
			err := instancetype.ValidSnapName(snapName)
			if err != nil {
				return nil, nil, fmt.Errorf("Invalid snapshot name %q: %w", snapName, err)
			}

			snapVol, _ := vol.NewSnapshot(snapName)
			snapDir := "snapshots"
			srcFilePrefix := snapName
			switch vol.volType {
			case VolumeTypeVM:
				snapDir = "virtual-machine-snapshots"
				if vol.contentType == ContentTypeFS {
					srcFilePrefix = snapName + "-config"
				}

			case VolumeTypeCustom:
				snapDir = "volume-snapshots"
			}

			srcFilePrefix = filepath.Join(snapDir, srcFilePrefix)
			err = unpackVolume(snapVol, srcFilePrefix)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	// Extract main volume.
	srcFilePrefix := "container"
	switch vol.volType {
	case VolumeTypeVM:
		if vol.contentType == ContentTypeFS {
			srcFilePrefix = "virtual-machine-config"
		} else {
			srcFilePrefix = "virtual-machine"
		}

	case VolumeTypeCustom:
		srcFilePrefix = "volume"
	}

	err = unpackVolume(vol.Volume, srcFilePrefix)
	if err != nil {
		return nil, nil, err
	}

	for _, copyOp := range copyOps {
		err = d.setSubvolumeReadonlyProperty(copyOp.src, false)
		if err != nil {
			return nil, nil, err
		}

		// Clear the target for the subvol to use.
		_ = os.Remove(copyOp.dest)

		// Move unpacked subvolume into its final location.
		err = os.Rename(copyOp.src, copyOp.dest)
		if err != nil {
			return nil, nil, err
		}
	}

	// Restore readonly property on subvolumes that need it.
	for _, subVol := range optimizedHeader.Subvolumes {
		if !subVol.Readonly {
			continue // All subvolumes are made writable during unpack process so we can skip these.
		}

		v := vol.Volume
		if subVol.Snapshot != "" {
			v, _ = vol.NewSnapshot(subVol.Snapshot)
		}

		path := filepath.Join(v.MountPath(), subVol.Path)
		d.logger.Debug("Setting subvolume readonly", logger.Ctx{"name": v.name, "path": path})
		err = d.setSubvolumeReadonlyProperty(path, true)
		if err != nil {
			return nil, nil, err
		}
	}

	revert.Success()
	return nil, revertHook, nil
}

// createVolumeFromCopy creates a volume from copy by snapshotting the parent volume.
// It also copies the source volume's snapshots and supports refreshing an already existing volume.
func (d *btrfs) createVolumeFromCopy(vol VolumeCopy, srcVol VolumeCopy, allowInconsistent bool, refresh bool, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	// Scan source for subvolumes (so we can apply the readonly properties on the new volume).
	subVols, err := d.getSubvolumesMetaData(srcVol.Volume)
	if err != nil {
		return err
	}

	target := vol.MountPath()

	// In case of refresh first delete the main volume.
	if refresh {
		err := d.deleteSubvolume(target, true)
		if err != nil {
			return err
		}
	}

	// Recursively copy the main volume.
	cleanup, err := d.snapshotSubvolume(srcVol.MountPath(), target, true)
	if err != nil {
		return err
	}

	if cleanup != nil {
		revert.Add(cleanup)
	}

	// Restore readonly property on subvolumes in reverse order (except root which should be left writable).
	subVolCount := len(subVols)
	for i := range subVols {
		i = subVolCount - 1 - i
		subVol := subVols[i]
		if subVol.Readonly && subVol.Path != string(filepath.Separator) {
			targetSubVolPath := filepath.Join(target, subVol.Path)
			err = d.setSubvolumeReadonlyProperty(targetSubVolPath, true)
			if err != nil {
				return err
			}
		}
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	err = d.SetVolumeQuota(vol.Volume, vol.config["size"], false, op)
	if err != nil {
		return err
	}

	// Fixup permissions after snapshot created.
	err = vol.EnsureMountPath()
	if err != nil {
		return err
	}

	var snapshots []string

	// Get snapshot list if copying snapshots.
	if len(vol.Snapshots) > 0 && !srcVol.IsSnapshot() {
		// Get the list of source snapshots.
		snapshots, err = d.VolumeSnapshots(srcVol.Volume, op)
		if err != nil {
			return err
		}
	}

	// Copy any snapshots needed.
	if len(snapshots) > 0 {
		// Create the parent directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		// Get the list of target volume snapshots.
		targetSnapshots, err := d.VolumeSnapshots(vol.Volume, op)
		if err != nil {
			return err
		}

		// Copy the snapshots.
		for _, snapName := range snapshots {
			if refresh {
				found := false
				// Use the list of target volume's snapshots to identify the ones that require refresh.
				for _, targetSnapshot := range vol.Snapshots {
					_, targetSnapshotName, _ := api.GetParentAndSnapshotName(targetSnapshot.name)
					if snapName == targetSnapshotName {
						found = true
					}
				}

				// Skip snapshots that shouldn't be refreshed on the target volume.
				// This could be either because the snapshot itself isn't in the list of target volume snapshots
				// inside of the DB or the snapshot already exists on the target volume.
				if !found || slices.Contains(targetSnapshots, snapName) {
					continue
				}
			}

			srcSnapshot := GetVolumeMountPath(d.name, srcVol.volType, GetSnapshotVolumeName(srcVol.name, snapName))
			dstSnapshot := GetVolumeMountPath(d.name, vol.volType, GetSnapshotVolumeName(vol.name, snapName))

			cleanup, err := d.snapshotSubvolume(srcSnapshot, dstSnapshot, true)
			if err != nil {
				return err
			}

			if cleanup != nil {
				revert.Add(cleanup)
			}

			err = d.setSubvolumeReadonlyProperty(dstSnapshot, true)
			if err != nil {
				return err
			}

			revert.Add(func() { _ = d.deleteSubvolume(dstSnapshot, true) })
		}
	}

	revert.Success()
	return nil
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *btrfs) CreateVolumeFromCopy(vol VolumeCopy, srcVol VolumeCopy, allowInconsistent bool, op *operations.Operation) error {
	return d.createVolumeFromCopy(vol, srcVol, allowInconsistent, false, op)
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *btrfs) CreateVolumeFromMigration(vol VolumeCopy, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volTargetArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volTargetArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		_, err := genericVFSCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
		return err
	} else if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_BTRFS {
		return ErrNotSupported
	}

	var migrationHeader BTRFSMetaDataHeader

	// List of subvolumes to be synced. This is sent back to the source.
	var syncSubvolumes []BTRFSSubVolume

	// Inspect negotiated features to see if we are expecting to get a metadata migration header frame.
	if slices.Contains(volTargetArgs.MigrationType.Features, migration.BTRFSFeatureMigrationHeader) {
		buf, err := io.ReadAll(conn)
		if err != nil {
			return fmt.Errorf("Failed reading BTRFS migration header: %w", err)
		}

		err = json.Unmarshal(buf, &migrationHeader)
		if err != nil {
			return fmt.Errorf("Failed decoding BTRFS migration header: %w", err)
		}

		d.logger.Debug("Received BTRFS migration meta data header", logger.Ctx{"name": vol.name})
	} else {
		// Populate the migrationHeader subvolumes with root volumes only to support older LXD sources.
		for _, snapName := range volTargetArgs.Snapshots {
			migrationHeader.Subvolumes = append(migrationHeader.Subvolumes, BTRFSSubVolume{
				Snapshot: snapName,
				Path:     string(filepath.Separator),
				Readonly: true, // Snapshots are made readonly.
			})
		}

		migrationHeader.Subvolumes = append(migrationHeader.Subvolumes, BTRFSSubVolume{
			Snapshot: "",
			Path:     string(filepath.Separator),
			Readonly: false,
		})
	}

	if volTargetArgs.Refresh && slices.Contains(volTargetArgs.MigrationType.Features, migration.BTRFSFeatureSubvolumeUUIDs) {
		snapshots, err := d.volumeSnapshotsSorted(vol.Volume, op)
		if err != nil {
			return err
		}

		// Reset list of snapshots which are to be received.
		volTargetArgs.Snapshots = []string{}

		// Map of local subvolumes with their received UUID.
		localSubvolumes := make(map[string]string)

		for _, snap := range snapshots {
			snapVol, _ := vol.NewSnapshot(snap)

			receivedUUID, err := d.getSubVolumeReceivedUUID(snapVol)
			if err != nil {
				return err
			}

			localSubvolumes[snap] = receivedUUID
		}

		// Figure out which snapshots need to be copied by comparing the UUIDs and received UUIDs from the migration header.
		for _, migrationSnap := range migrationHeader.Subvolumes {
			receivedUUID, ok := localSubvolumes[migrationSnap.Snapshot]
			// Skip this snapshot as it exists on both the source and target, and has the same GUID.
			if ok && receivedUUID == migrationSnap.UUID {
				continue
			}

			if migrationSnap.Path == "/" && migrationSnap.Snapshot != "" {
				volTargetArgs.Snapshots = append(volTargetArgs.Snapshots, migrationSnap.Snapshot)
			}

			syncSubvolumes = append(syncSubvolumes, BTRFSSubVolume{Path: migrationSnap.Path, Snapshot: migrationSnap.Snapshot, UUID: migrationSnap.UUID})
		}

		migrationHeader = BTRFSMetaDataHeader{Subvolumes: syncSubvolumes}

		headerJSON, err := json.Marshal(migrationHeader)
		if err != nil {
			return fmt.Errorf("Failed encoding BTRFS migration header: %w", err)
		}

		_, err = conn.Write(headerJSON)
		if err != nil {
			return fmt.Errorf("Failed sending BTRFS migration header: %w", err)
		}

		err = conn.Close() // End the frame.
		if err != nil {
			return fmt.Errorf("Failed closing BTRFS migration header frame: %w", err)
		}

		d.logger.Debug("Sent BTRFS migration meta data header", logger.Ctx{"name": vol.name, "header": migrationHeader})
	} else {
		syncSubvolumes = migrationHeader.Subvolumes
	}

	return d.createVolumeFromMigrationOptimized(vol.Volume, conn, volTargetArgs, preFiller, syncSubvolumes, op)
}

func (d *btrfs) createVolumeFromMigrationOptimized(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, subvolumes []BTRFSSubVolume, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	type btrfsCopyOp struct {
		src          string
		dest         string
		receivedUUID string
	}

	// copyOps represents copy operations which need to take place once *all* subvolumes have been
	// received. We don't use a map as the order should be kept.
	copyOps := []btrfsCopyOp{}

	// receiveVolume receives all subvolumes in a LXD volume from the source.
	receiveVolume := func(v Volume, receivePath string) error {
		_, snapName, _ := api.GetParentAndSnapshotName(v.name)

		// Setup progress tracking.
		var wrapper *ioprogress.ProgressTracker
		if volTargetArgs.TrackProgress {
			wrapper = migration.ProgressTracker(op, "fs_progress", v.name)
		}

		for _, subVol := range subvolumes {
			if subVol.Snapshot != snapName {
				continue // Skip any subvolumes that dont belong to our volume (empty for main).
			}

			receivePath := filepath.Join(receivePath, snapName)

			err := os.MkdirAll(receivePath, 0100)
			if err != nil {
				return fmt.Errorf("Failed creating %q: %w", receivePath, err)
			}

			subVolTargetPath := filepath.Join(v.MountPath(), subVol.Path)
			d.logger.Debug("Receiving volume", logger.Ctx{"name": v.name, "receivePath": receivePath, "path": subVolTargetPath})

			subVolRecvPath, err := d.receiveSubVolume(conn, receivePath, wrapper)
			if err != nil {
				return err
			}

			receivedVol := Volume{
				pool:            d.name,
				mountCustomPath: subVolRecvPath,
			}

			UUID, err := d.getSubVolumeReceivedUUID(receivedVol)
			if err != nil {
				return fmt.Errorf("Failed getting UUID: %w", err)
			}

			// Record the copy operations we need to do after having received all subvolumes.
			copyOps = append(copyOps, btrfsCopyOp{
				src:          subVolRecvPath,
				dest:         subVolTargetPath,
				receivedUUID: UUID,
			})
		}

		return nil
	}

	// Get instances directory (e.g. /var/lib/lxd/storage-pools/btrfs/containers).
	instancesPath := GetVolumeMountPath(d.name, vol.volType, "")

	// Create a temporary directory which will act as the parent directory of the received ro snapshot.
	tmpVolumesMountPoint, err := os.MkdirTemp(instancesPath, "migration.")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory under %q: %w", instancesPath, err)
	}

	defer func() { _ = os.RemoveAll(tmpVolumesMountPoint) }()

	err = os.Chmod(tmpVolumesMountPoint, 0100)
	if err != nil {
		return fmt.Errorf("Failed to chmod %q: %w", tmpVolumesMountPoint, err)
	}

	// Handle btrfs send/receive migration.
	if !volTargetArgs.VolumeOnly && len(volTargetArgs.Snapshots) > 0 {
		// Create the parent directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		revert.Add(func() { _ = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name) })

		// Transfer the snapshots.
		for _, snapName := range volTargetArgs.Snapshots {
			snapVol, _ := vol.NewSnapshot(snapName)
			err = receiveVolume(snapVol, tmpVolumesMountPoint)
			if err != nil {
				return err
			}
		}
	}

	// Receive main volume.
	err = receiveVolume(vol, tmpVolumesMountPoint)
	if err != nil {
		return err
	}

	if volTargetArgs.Refresh {
		// Delete main volume after receiving it.
		err = d.deleteSubvolume(vol.MountPath(), true)
		if err != nil {
			return err
		}
	}

	// Make all received subvolumes read-write and move them to their final destination
	for _, op := range copyOps {
		err = d.setSubvolumeReadonlyProperty(op.src, false)
		if err != nil {
			return err
		}

		// Clear the target for the subvol to use.
		_ = os.Remove(op.dest)

		err = os.Rename(op.src, op.dest)
		if err != nil {
			return err
		}

		// This sets the "Received UUID" field on the subvolume.
		// When making the received subvolume read-write before moving it to its final location,
		// this information is lost (by design). However, this causes issues when performing
		// incremental streams (error: "cannot find parent subvolume").
		// Setting the "Received UUID" field to the value of the received subvolume (before making
		// it rw) solves this issue.
		err = setReceivedUUID(op.dest, op.receivedUUID)
		if err != nil {
			return fmt.Errorf("Failed setting received UUID: %w", err)
		}
	}

	// Restore readonly property on subvolumes that need it.
	for _, subVol := range subvolumes {
		if !subVol.Readonly {
			continue // All subvolumes are made writable during receive process so we can skip these.
		}

		v := vol
		if subVol.Snapshot != "" {
			v, _ = vol.NewSnapshot(subVol.Snapshot)
		}

		path := filepath.Join(v.MountPath(), subVol.Path)
		d.logger.Debug("Setting subvolume readonly", logger.Ctx{"name": v.name, "path": path})
		err = d.setSubvolumeReadonlyProperty(path, true)
		if err != nil {
			return err
		}
	}

	if vol.contentType == ContentTypeFS {
		// Apply the size limit.
		err = d.SetVolumeQuota(vol, vol.ConfigSize(), false, op)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *btrfs) RefreshVolume(vol VolumeCopy, srcVol VolumeCopy, refreshSnapshots []string, allowInconsistent bool, op *operations.Operation) error {
	return d.createVolumeFromCopy(vol, srcVol, allowInconsistent, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *btrfs) DeleteVolume(vol Volume, op *operations.Operation) error {
	// Check that we don't have snapshots.
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return errors.New("Cannot remove a volume that has snapshots")
	}

	volName := vol.name

	if vol.volType == VolumeTypeCustom && vol.contentType == ContentTypeISO {
		volName = vol.name + btrfsISOVolSuffix
	}

	// If the volume doesn't exist, then nothing more to do.
	volPath := GetVolumeMountPath(d.name, vol.volType, volName)
	if !shared.PathExists(volPath) {
		return nil
	}

	// Delete the volume (and any subvolumes).
	err = d.deleteSubvolume(volPath, true)
	if err != nil {
		return err
	}

	// Although the volume snapshot directory should already be removed, lets remove it here
	// to just in case the top-level directory is left.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, volName)
	if err != nil {
		return err
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *btrfs) HasVolume(vol Volume) (bool, error) {
	return genericVFSHasVolume(vol)
}

// ValidateVolume validates the supplied volume config.
func (d *btrfs) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	return d.validateVolume(vol, nil, removeUnknownKeys)
}

// UpdateVolume applies config changes to the volume.
func (d *btrfs) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	newSize, sizeChanged := changedConfig["size"]
	if sizeChanged {
		err := d.SetVolumeQuota(vol, newSize, false, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetVolumeUsage returns the disk space used by the volume.
func (d *btrfs) GetVolumeUsage(vol Volume) (int64, error) {
	// Attempt to get the qgroup information.
	_, usage, err := d.getQGroup(vol.MountPath())
	if err != nil {
		if err == errBtrfsNoQuota {
			return -1, ErrNotSupported
		}

		return -1, err
	}

	return usage, nil
}

// SetVolumeQuota applies a size limit on volume.
// Does nothing if supplied with an empty/zero size for block volumes, and for filesystem volumes removes quota.
func (d *btrfs) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Convert to bytes.
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	// For VM block files, resize the file if needed.
	if vol.contentType == ContentTypeBlock {
		// Do nothing if size isn't specified.
		if sizeBytes <= 0 {
			return nil
		}

		rootBlockPath, err := d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}

		// Pass VolumeTypeImage as unsupported resize type, as if the image volume doesn't match the
		// requested size and allowUnsafeResize=false, this needs to be rejected back to caller as
		// ErrNotSupported so that the caller can take the appropriate action. In the case of optimized
		// image volumes, this will cause the image volume to be deleted and regenerated with the new size.
		// In other cases this is probably a bug and the operation should fail anyway.
		resized, err := ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, allowUnsafeResize, VolumeTypeImage)
		if err != nil {
			return err
		}

		// Move the GPT alt header to end of disk if needed and resize has taken place (not needed in
		// unsafe resize mode as it is expected the caller will do all necessary post resize actions
		// themselves).
		if vol.IsVMBlock() && resized && !allowUnsafeResize {
			err = d.moveGPTAltHeader(rootBlockPath)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// For non-VM block volumes, set filesystem quota.
	volPath := vol.MountPath()

	// Try to locate an existing quota group.
	qgroup, _, err := d.getQGroup(volPath)
	if err != nil && !d.state.OS.RunningInUserNS {
		// If quotas are disabled, attempt to enable them.
		if err == errBtrfsNoQuota {
			if sizeBytes <= 0 {
				// Nothing to do if the quota is being removed and we don't currently have quota.
				return nil
			}

			path := GetPoolMountPath(d.name)

			_, err = shared.RunCommandContext(context.TODO(), "btrfs", "quota", "enable", path)
			if err != nil {
				return err
			}

			// Try again.
			qgroup, _, err = d.getQGroup(volPath)
		}

		// If there's no qgroup, attempt to create one.
		if err == errBtrfsNoQGroup {
			// Find the volume ID.
			var output string
			output, err = shared.RunCommandContext(context.TODO(), "btrfs", "subvolume", "show", volPath)
			if err != nil {
				return fmt.Errorf("Failed to get subvol information: %w", err)
			}

			id := ""
			for line := range strings.SplitSeq(output, "\n") {
				_, value, found := strings.Cut(line, "Subvolume ID:")
				if found {
					id = strings.TrimSpace(value)
					break
				}
			}

			if id == "" {
				return fmt.Errorf("Failed to find subvolume id for %q", volPath)
			}

			// Create a qgroup.
			_, err = shared.RunCommandContext(context.TODO(), "btrfs", "qgroup", "create", "0/"+id, volPath)
			if err != nil {
				return err
			}

			// Try to get the qgroup again.
			qgroup, _, err = d.getQGroup(volPath)
		}

		if err != nil {
			return err
		}
	}

	// Modify the limit.
	if sizeBytes > 0 {
		// Custom handling for filesystem volume associated with a VM.
		if vol.volType == VolumeTypeVM && shared.PathExists(filepath.Join(volPath, genericVolumeDiskFile)) {
			// Get the size of the VM image.
			blockSize, err := block.DiskSizeBytes(filepath.Join(volPath, genericVolumeDiskFile))
			if err != nil {
				return err
			}

			// Add that to the requested filesystem size (to ignore it from the quota).
			sizeBytes += blockSize
			d.logger.Debug("Accounting for VM image file size", logger.Ctx{"sizeBytes": sizeBytes})
		}

		// Apply the limit to referenced data in qgroup.
		_, err = shared.RunCommandContext(context.TODO(), "btrfs", "qgroup", "limit", strconv.FormatInt(sizeBytes, 10), qgroup, volPath)
		if err != nil {
			return err
		}

		// Remove any former exclusive data limit.
		_, err = shared.RunCommandContext(context.TODO(), "btrfs", "qgroup", "limit", "-e", "none", qgroup, volPath)
		if err != nil {
			return err
		}
	} else if qgroup != "" {
		// Remove all limits.
		_, err = shared.RunCommandContext(context.TODO(), "btrfs", "qgroup", "limit", "none", qgroup, volPath)
		if err != nil {
			return err
		}

		_, err = shared.RunCommandContext(context.TODO(), "btrfs", "qgroup", "limit", "-e", "none", qgroup, volPath)
		if err != nil {
			return err
		}
	}

	return nil
}

// GetVolumeDiskPath returns the location and file format of a disk volume.
func (d *btrfs) GetVolumeDiskPath(vol Volume) (string, error) {
	return genericVFSGetVolumeDiskPath(vol)
}

// ListVolumes returns a list of LXD volumes in storage pool.
func (d *btrfs) ListVolumes() ([]Volume, error) {
	return genericVFSListVolumes(d)
}

// MountVolume simulates mounting a volume.
func (d *btrfs) MountVolume(vol Volume, op *operations.Operation) error {
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	// Don't attempt to modify the permission of an existing custom volume root.
	// A user inside the instance may have modified this and we don't want to reset it on restart.
	if !shared.PathExists(vol.MountPath()) || vol.volType != VolumeTypeCustom {
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}
	}

	vol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolume() when done.
	return nil
}

// UnmountVolume simulates unmounting a volume.
// As driver doesn't have volumes to unmount it returns false indicating the volume was already unmounted.
func (d *btrfs) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock, err := vol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	refCount := vol.MountRefCountDecrement()
	if refCount > 0 {
		d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": vol.name, "refCount": refCount})
		return false, ErrInUse
	}

	return false, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *btrfs) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return genericVFSRenameVolume(d, vol, newVolName, op)
}

// readonlySnapshot creates a readonly snapshot.
// Returns a revert fail function that can be used to undo this function if a subsequent step fails.
func (d *btrfs) readonlySnapshot(vol Volume) (string, revert.Hook, error) {
	revert := revert.New()
	defer revert.Fail()

	sourcePath := vol.MountPath()
	poolPath := GetPoolMountPath(d.name)
	tmpDir, err := os.MkdirTemp(poolPath, "backup.")
	if err != nil {
		return "", nil, err
	}

	revert.Add(func() {
		_ = os.RemoveAll(tmpDir)
	})

	err = os.Chmod(tmpDir, 0100)
	if err != nil {
		return "", nil, err
	}

	mountPath := filepath.Join(tmpDir, vol.name)

	cleanup, err := d.snapshotSubvolume(sourcePath, mountPath, true)
	if err != nil {
		return "", nil, err
	}

	if cleanup != nil {
		revert.Add(cleanup)
	}

	err = d.setSubvolumeReadonlyProperty(mountPath, true)
	if err != nil {
		return "", nil, err
	}

	d.logger.Debug("Created read-only backup snapshot", logger.Ctx{"sourcePath": sourcePath, "path": mountPath})

	cleanup = revert.Clone().Fail
	revert.Success()
	return mountPath, cleanup, nil
}

// MigrateVolume sends a volume for migration.
func (d *btrfs) MigrateVolume(vol VolumeCopy, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	// Handle simple rsync and block_and_rsync through generic.
	if volSrcArgs.MigrationType.FSType == migration.MigrationFSType_RSYNC || volSrcArgs.MigrationType.FSType == migration.MigrationFSType_BLOCK_AND_RSYNC {
		// If volume is filesystem type and is not already a snapshot, create a fast snapshot to ensure migration is consistent.
		// TODO add support for temporary snapshots of block volumes here.
		if vol.contentType == ContentTypeFS && !vol.IsSnapshot() {
			snapshotPath, cleanup, err := d.readonlySnapshot(vol.Volume)
			if err != nil {
				return err
			}

			// Clean up the snapshot.
			defer cleanup()

			// Set the path of the volume to the path of the fast snapshot so the migration reads from there instead.
			vol.mountCustomPath = snapshotPath
		}

		return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
	} else if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_BTRFS {
		return ErrNotSupported
	}

	// Handle btrfs send/receive migration.
	if volSrcArgs.MultiSync || volSrcArgs.FinalSync {
		// This is not needed if the migration is performed using btrfs send/receive.
		return errors.New("MultiSync should not be used with optimized migration")
	}

	var snapshots []string
	var err error

	if !volSrcArgs.VolumeOnly {
		// Generate restoration header, containing info on the subvolumes and how they should be restored.
		snapshots, err = d.volumeSnapshotsSorted(vol.Volume, op)
		if err != nil {
			return err
		}
	}

	migrationHeader, err := d.restorationHeader(vol.Volume, snapshots)
	if err != nil {
		return err
	}

	// If we haven't negotiated subvolume support, check if we have any subvolumes in source and fail,
	// otherwise we would end up not materialising all of the source's files on the target.
	if !slices.Contains(volSrcArgs.MigrationType.Features, migration.BTRFSFeatureMigrationHeader) || !slices.Contains(volSrcArgs.MigrationType.Features, migration.BTRFSFeatureSubvolumes) {
		for _, subVol := range migrationHeader.Subvolumes {
			if subVol.Path != string(filepath.Separator) {
				return errors.New("Subvolumes detected in source but target does not support receiving subvolumes")
			}
		}
	}

	// Send metadata migration header frame with subvolume info if we have negotiated that feature.
	if slices.Contains(volSrcArgs.MigrationType.Features, migration.BTRFSFeatureMigrationHeader) {
		headerJSON, err := json.Marshal(migrationHeader)
		if err != nil {
			return fmt.Errorf("Failed encoding BTRFS migration header: %w", err)
		}

		_, err = conn.Write(headerJSON)
		if err != nil {
			return fmt.Errorf("Failed sending BTRFS migration header: %w", err)
		}

		err = conn.Close() // End the frame.
		if err != nil {
			return fmt.Errorf("Failed closing BTRFS migration header frame: %w", err)
		}

		d.logger.Debug("Sent migration meta data header", logger.Ctx{"name": vol.name})
	}

	if volSrcArgs.Refresh && slices.Contains(volSrcArgs.MigrationType.Features, migration.BTRFSFeatureSubvolumeUUIDs) {
		migrationHeader = &BTRFSMetaDataHeader{}

		buf, err := io.ReadAll(conn)
		if err != nil {
			return fmt.Errorf("Failed reading BTRFS migration header: %w", err)
		}

		err = json.Unmarshal(buf, &migrationHeader)
		if err != nil {
			return fmt.Errorf("Failed decoding BTRFS migration header: %w", err)
		}

		d.logger.Debug("Received BTRFS migration meta data header", logger.Ctx{"name": vol.name})

		volSrcArgs.Snapshots = []string{}

		// Override volSrcArgs.Snapshots to only include snapshots which need to be sent.
		for _, snap := range migrationHeader.Subvolumes {
			if snap.Path == "/" && snap.Snapshot != "" {
				volSrcArgs.Snapshots = append(volSrcArgs.Snapshots, snap.Snapshot)
			}
		}
	}

	return d.migrateVolumeOptimized(vol.Volume, conn, volSrcArgs, migrationHeader.Subvolumes, op)
}

func (d *btrfs) migrateVolumeOptimized(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, subvolumes []BTRFSSubVolume, op *operations.Operation) error {
	// sendVolume sends a volume and its subvolumes (if negotiated subvolumes feature) to recipient.
	sendVolume := func(v Volume, sourcePrefix string, parentPrefix string) error {
		snapName := "" // Default to empty (sending main volume) from migrationHeader.Subvolumes.

		// Detect if we are sending a snapshot by comparing to main volume name.
		// We can't only use IsSnapshot() as the main vol may itself be a snapshot.
		if v.IsSnapshot() && v.name != vol.name {
			_, snapName, _ = api.GetParentAndSnapshotName(v.name)
		}

		// Setup progress tracking.
		var wrapper *ioprogress.ProgressTracker
		if volSrcArgs.TrackProgress {
			wrapper = migration.ProgressTracker(op, "fs_progress", v.name)
		}

		sentVols := 0

		// Send volume (and any subvolumes if supported) to target.
		//revive:disable:defer Allow defer inside a loop.
		for _, subVolume := range subvolumes {
			if subVolume.Snapshot != snapName {
				continue // Only sending subvolumes related to snapshot name (empty for main vol).
			}

			if subVolume.Path != string(filepath.Separator) && !slices.Contains(volSrcArgs.MigrationType.Features, migration.BTRFSFeatureSubvolumes) {
				continue // Skip sending subvolumes of volume if subvolumes feature not negotiated.
			}

			// Detect if parent subvolume exists, and if so use it for differential.
			parentPath := ""
			if parentPrefix != "" && d.isSubvolume(filepath.Join(parentPrefix, subVolume.Path)) {
				parentPath = filepath.Join(parentPrefix, subVolume.Path)

				// Set parent subvolume readonly if needed so we can send the subvolume.
				if !btrfsSubVolumeIsRo(parentPath) {
					err := d.setSubvolumeReadonlyProperty(parentPath, true)
					if err != nil {
						return err
					}

					defer func() { _ = d.setSubvolumeReadonlyProperty(parentPath, false) }()
				}
			}

			// Set subvolume readonly if needed so we can send it.
			sourcePath := filepath.Join(sourcePrefix, subVolume.Path)
			if !btrfsSubVolumeIsRo(sourcePath) {
				err := d.setSubvolumeReadonlyProperty(sourcePath, true)
				if err != nil {
					return err
				}

				defer func() { _ = d.setSubvolumeReadonlyProperty(sourcePath, false) }()
			}

			d.logger.Debug("Sending subvolume", logger.Ctx{"name": v.name, "source": sourcePath, "parent": parentPath, "path": subVolume.Path})
			err := d.sendSubvolume(sourcePath, parentPath, conn, wrapper)
			if err != nil {
				return fmt.Errorf("Failed sending volume %v:%s: %w", v.name, subVolume.Path, err)
			}

			sentVols++
		}

		// Ensure we found and sent at least root subvolume of the volume requested.
		if sentVols < 1 {
			return fmt.Errorf("No matching subvolume(s) for %q found in subvolumes list", v.name)
		}

		return nil
	}

	// Transfer the snapshots (and any subvolumes if supported) to target first.
	lastVolPath := "" // Used as parent for differential transfers.

	if !vol.IsSnapshot() && !volSrcArgs.VolumeOnly {
		snapshots, err := vol.Snapshots(op)
		if err != nil {
			return err
		}

		if volSrcArgs.Refresh {
			for i, snap := range snapshots {
				if i == 0 {
					continue
				}

				_, snapName, _ := api.GetParentAndSnapshotName(snap.name)

				if len(volSrcArgs.Snapshots) > 0 && snapName == volSrcArgs.Snapshots[0] {
					lastVolPath = snapshots[i-1].MountPath()
					break
				}
			}
		}

		for _, snapName := range volSrcArgs.Snapshots {
			snapVol, _ := vol.NewSnapshot(snapName)
			err := sendVolume(snapVol, snapVol.MountPath(), lastVolPath)
			if err != nil {
				return err
			}

			lastVolPath = snapVol.MountPath()
		}

		// If no snapshots are to be copied (because they are on the target already), but snapshots
		// exist on the source, use the latest snapshot as the parent in order to speed up
		// optimized refresh.
		if volSrcArgs.Refresh && len(volSrcArgs.Snapshots) == 0 && len(snapshots) > 0 {
			lastVolPath = snapshots[len(snapshots)-1].MountPath()
		}
	}

	// Get instances directory (e.g. /var/lib/lxd/storage-pools/btrfs/containers).
	instancesPath := GetVolumeMountPath(d.name, vol.volType, "")

	// Create a temporary directory which will act as the parent directory of the read-only snapshot.
	tmpVolumesMountPoint, err := os.MkdirTemp(instancesPath, "migration.")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory under %q: %w", instancesPath, err)
	}

	defer func() { _ = os.RemoveAll(tmpVolumesMountPoint) }()

	err = os.Chmod(tmpVolumesMountPoint, 0100)
	if err != nil {
		return fmt.Errorf("Failed to chmod %q: %w", tmpVolumesMountPoint, err)
	}

	// Make recursive read-only snapshot of the subvolume as writable subvolumes cannot be sent.
	migrationSendSnapshotPrefix := filepath.Join(tmpVolumesMountPoint, ".migration-send")
	_, err = d.snapshotSubvolume(vol.MountPath(), migrationSendSnapshotPrefix, true)
	if err != nil {
		return err
	}

	defer func() { _ = d.deleteSubvolume(migrationSendSnapshotPrefix, true) }()

	// Send main volume (and any subvolumes if supported) to target.
	return sendVolume(vol, migrationSendSnapshotPrefix, lastVolPath)
}

// BackupVolume copies a volume (and optionally its snapshots) to a specified target path.
// This driver does not support optimized backups.
func (d *btrfs) BackupVolume(vol VolumeCopy, tarWriter *instancewriter.InstanceTarWriter, optimized bool, snapshots []string, op *operations.Operation) error {
	// Handle the non-optimized tarballs through the generic packer.
	if !optimized {
		// Because the generic backup method will not take a consistent backup if files are being modified
		// as they are copied to the tarball, as BTRFS allows us to take a quick snapshot without impacting
		// the parent volume we do so here to ensure the backup taken is consistent.
		if vol.contentType == ContentTypeFS {
			snapshotPath, cleanup, err := d.readonlySnapshot(vol.Volume)
			if err != nil {
				return err
			}

			// Clean up the snapshot.
			defer cleanup()

			// Set the path of the volume to the path of the fast snapshot so the migration reads from there instead.
			vol.mountCustomPath = snapshotPath
		}

		return genericVFSBackupVolume(d, vol, tarWriter, snapshots, op)
	}

	// Optimized backup.

	if len(snapshots) > 0 {
		// Check requested snapshot match those in storage.
		err := d.CheckVolumeSnapshots(vol.Volume, vol.Snapshots, op)
		if err != nil {
			return err
		}
	}

	// Generate driver restoration header.
	optimizedHeader, err := d.restorationHeader(vol.Volume, snapshots)
	if err != nil {
		return err
	}

	// Convert to YAML.
	optimizedHeaderYAML, err := yaml.Marshal(&optimizedHeader)
	if err != nil {
		return err
	}

	r := bytes.NewReader(optimizedHeaderYAML)

	indexFileInfo := instancewriter.FileInfo{
		FileName:    "backup/optimized_header.yaml",
		FileSize:    int64(len(optimizedHeaderYAML)),
		FileMode:    0644,
		FileModTime: time.Now(),
	}

	// Write to tarball.
	err = tarWriter.WriteFileFromReader(r, &indexFileInfo)
	if err != nil {
		return err
	}

	// sendToFile sends a subvolume to backup file.
	sendToFile := func(path string, parent string, fileName string) error {
		// Prepare btrfs send arguments.
		args := []string{"send"}
		if parent != "" {
			args = append(args, "-p", parent)
		}

		args = append(args, path)

		// Create temporary file to store output of btrfs send.
		tmpFile, err := os.CreateTemp(d.state.BackupsStoragePath(), backup.WorkingDirPrefix+"_btrfs")
		if err != nil {
			return fmt.Errorf("Failed to open temporary file for BTRFS backup: %w", err)
		}

		defer func() { _ = tmpFile.Close() }()
		defer func() { _ = os.Remove(tmpFile.Name()) }()

		// Write the subvolume to the file.
		d.logger.Debug("Generating optimized volume file", logger.Ctx{"sourcePath": path, "parent": parent, "file": tmpFile.Name(), "name": fileName})
		err = shared.RunCommandWithFds(d.state.ShutdownCtx, nil, tmpFile, "btrfs", args...)
		if err != nil {
			return err
		}

		// Get info (importantly size) of the generated file for tarball header.
		tmpFileInfo, err := os.Lstat(tmpFile.Name())
		if err != nil {
			return err
		}

		err = tarWriter.WriteFile(fileName, tmpFile.Name(), tmpFileInfo, false)
		if err != nil {
			return err
		}

		return tmpFile.Close()
	}

	// addVolume adds a volume and its subvolumes to backup file.
	addVolume := func(v Volume, sourcePrefix string, parentPrefix string, fileNamePrefix string) error {
		snapName := "" // Default to empty (sending main volume) from migrationHeader.Subvolumes.

		// Detect if we are adding a snapshot by comparing to main volume name.
		// We can't only use IsSnapshot() as the main vol may itself be a snapshot.
		if v.IsSnapshot() && v.name != vol.name {
			_, snapName, _ = api.GetParentAndSnapshotName(v.name)
		}

		sentVols := 0

		// Add volume (and any subvolumes if supported) to backup file.
		for _, subVolume := range optimizedHeader.Subvolumes {
			if subVolume.Snapshot != snapName {
				continue // Only add subvolumes related to snapshot name (empty for main vol).
			}

			// Detect if parent subvolume exists, and if so use it for differential.
			parentPath := ""
			if parentPrefix != "" && d.isSubvolume(filepath.Join(parentPrefix, subVolume.Path)) {
				parentPath = filepath.Join(parentPrefix, subVolume.Path)

				// Set parent subvolume readonly if needed so we can add the subvolume.
				if !btrfsSubVolumeIsRo(parentPath) {
					err = d.setSubvolumeReadonlyProperty(parentPath, true)
					if err != nil {
						return err
					}

					defer func() { _ = d.setSubvolumeReadonlyProperty(parentPath, false) }()
				}
			}

			// Set subvolume readonly if needed so we can add it.
			sourcePath := filepath.Join(sourcePrefix, subVolume.Path)
			if !btrfsSubVolumeIsRo(sourcePath) {
				err = d.setSubvolumeReadonlyProperty(sourcePath, true)
				if err != nil {
					return err
				}

				defer func() { _ = d.setSubvolumeReadonlyProperty(sourcePath, false) }()
			}

			// Default to no subvolume name for root subvolume to maintain backwards compatibility
			// with earlier optimized dump format. Although restoring this backup file on an earlier
			// system will not restore the subvolumes stored inside the backup.
			subVolName := ""
			if subVolume.Path != string(filepath.Separator) {
				// Encode the path of the subvolume (without the leading /) into the filename so
				// that we find the file from the optimized header's Path field on restore.
				subVolName = "_" + filesystem.PathNameEncode(strings.TrimPrefix(subVolume.Path, string(filepath.Separator)))
			}

			fileName := fileNamePrefix + subVolName + ".bin"
			err = sendToFile(sourcePath, parentPath, filepath.Join("backup", fileName))
			if err != nil {
				return fmt.Errorf("Failed adding volume %v:%s: %w", v.name, subVolume.Path, err)
			}

			sentVols++
		}

		// Ensure we found and sent at least root subvolume of the volume requested.
		if sentVols < 1 {
			return fmt.Errorf("No matching subvolume(s) for %q found in subvolumes list", v.name)
		}

		return nil
	}

	// Backup snapshots if populated.
	lastVolPath := "" // Used as parent for differential exports.
	for _, snapName := range snapshots {
		snapVol, _ := vol.NewSnapshot(snapName)

		// Make a binary btrfs backup.
		snapDir := "snapshots"
		fileName := snapName
		switch vol.volType {
		case VolumeTypeVM:
			snapDir = "virtual-machine-snapshots"
			if vol.contentType == ContentTypeFS {
				fileName = snapName + "-config"
			}

		case VolumeTypeCustom:
			snapDir = "volume-snapshots"
		}

		fileNamePrefix := filepath.Join(snapDir, fileName)
		err := addVolume(snapVol, snapVol.MountPath(), lastVolPath, fileNamePrefix)
		if err != nil {
			return err
		}

		lastVolPath = snapVol.MountPath()
	}

	// Make a temporary copy of the instance.
	sourceVolume := vol.MountPath()
	instancesPath := GetVolumeMountPath(d.name, vol.volType, "")

	tmpInstanceMntPoint, err := os.MkdirTemp(instancesPath, "backup.")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory under %q: %w", instancesPath, err)
	}

	defer func() { _ = os.RemoveAll(tmpInstanceMntPoint) }()

	err = os.Chmod(tmpInstanceMntPoint, 0100)
	if err != nil {
		return fmt.Errorf("Failed to chmod %q: %w", tmpInstanceMntPoint, err)
	}

	// Create the read-only snapshot.
	targetVolume := tmpInstanceMntPoint + "/.backup"
	_, err = d.snapshotSubvolume(sourceVolume, targetVolume, true)
	if err != nil {
		return err
	}

	defer func() { _ = d.deleteSubvolume(targetVolume, true) }()

	err = d.setSubvolumeReadonlyProperty(targetVolume, true)
	if err != nil {
		return err
	}

	// Dump the instance to a file.
	fileNamePrefix := "container"
	switch vol.volType {
	case VolumeTypeVM:
		if vol.contentType == ContentTypeFS {
			fileNamePrefix = "virtual-machine-config"
		} else {
			fileNamePrefix = "virtual-machine"
		}

	case VolumeTypeCustom:
		fileNamePrefix = "volume"
	}

	err = addVolume(vol.Volume, targetVolume, lastVolPath, fileNamePrefix)
	if err != nil {
		return err
	}

	// Ensure snapshot sub volumes are removed.
	err = d.deleteSubvolume(targetVolume, true)
	if err != nil {
		return err
	}

	return nil
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *btrfs) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := api.GetParentAndSnapshotName(snapVol.name)
	srcPath := GetVolumeMountPath(d.name, snapVol.volType, parentName)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	cleanup, err := d.snapshotSubvolume(srcPath, snapPath, true)
	if err != nil {
		return err
	}

	if cleanup != nil {
		revert.Add(cleanup)
	}

	err = d.setSubvolumeReadonlyProperty(snapPath, true)
	if err != nil {
		return err
	}

	// Set any subvolumes that were readonly in the source also readonly in the snapshot.
	srcVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)
	subVols, err := d.getSubvolumesMetaData(srcVol)
	if err != nil {
		return err
	}

	for _, subVol := range subVols {
		if subVol.Readonly {
			err = d.setSubvolumeReadonlyProperty(filepath.Join(snapPath, subVol.Path), true)
			if err != nil {
				return err
			}
		}
	}

	revert.Success()
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device. The volName and snapshotName
// must be bare names and should not be in the format "volume/snapshot".
func (d *btrfs) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	snapPath := snapVol.MountPath()

	// Delete the snapshot.
	err := d.deleteSubvolume(snapPath, true)
	if err != nil {
		return err
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	parentName, _, _ := api.GetParentAndSnapshotName(snapVol.name)
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// MountVolumeSnapshot sets up a read-only mount on top of the snapshot to avoid accidental modifications.
func (d *btrfs) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	snapPath := snapVol.MountPath()

	// Don't attempt to modify the permission of an existing custom volume root.
	// A user inside the instance may have modified this and we don't want to reset it on restart.
	if !shared.PathExists(snapPath) || snapVol.volType != VolumeTypeCustom {
		err := snapVol.EnsureMountPath()
		if err != nil {
			return err
		}
	}

	_, err = mountReadOnly(snapPath, snapPath)
	if err != nil {
		return err
	}

	snapVol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolumeSnapshot() when done.
	return nil
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
func (d *btrfs) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	refCount := snapVol.MountRefCountDecrement()
	if refCount > 0 {
		d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": snapVol.name, "refCount": refCount})
		return false, ErrInUse
	}

	snapPath := snapVol.MountPath()
	return forceUnmount(snapPath)
}

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *btrfs) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	return genericVFSVolumeSnapshots(d, vol, op)
}

// volumeSnapshotsSorted returns a list of snapshots for the volume (ordered by subvolume ID).
// Since the subvolume ID is incremental, this also represents the order of creation.
func (d *btrfs) volumeSnapshotsSorted(vol Volume, op *operations.Operation) ([]string, error) {
	stdout := bytes.Buffer{}

	err := shared.RunCommandWithFds(d.state.ShutdownCtx, nil, &stdout, "btrfs", "subvolume", "list", GetPoolMountPath(vol.pool))
	if err != nil {
		return nil, err
	}

	var snapshotNames []string

	snapshotPrefix := string(vol.volType) + "-snapshots/" + vol.name + "/"
	scanner := bufio.NewScanner(&stdout)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		if len(fields) != 9 {
			continue
		}

		if !strings.HasPrefix(fields[8], snapshotPrefix) {
			continue
		}

		// Exclude subvolumes of snapshots
		if strings.Contains(strings.TrimPrefix(fields[8], snapshotPrefix), "/") {
			continue
		}

		snapshotNames = append(snapshotNames, filepath.Base(fields[8]))
	}

	return snapshotNames, nil
}

// RestoreVolume restores a volume from a snapshot.
func (d *btrfs) RestoreVolume(vol Volume, snapVol Volume, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	_, snapshotName, _ := api.GetParentAndSnapshotName(snapVol.name)
	srcVol := NewVolume(d, d.name, vol.volType, vol.contentType, GetSnapshotVolumeName(vol.name, snapshotName), vol.config, vol.poolConfig)

	// Scan source for subvolumes (so we can apply the readonly properties on the restored snapshot).
	subVols, err := d.getSubvolumesMetaData(srcVol)
	if err != nil {
		return err
	}

	target := vol.MountPath()

	// Create a backup so we can revert.
	backupSubvolume := target + tmpVolSuffix
	err = os.Rename(target, backupSubvolume)
	if err != nil {
		return fmt.Errorf("Failed to rename %q to %q: %w", target, backupSubvolume, err)
	}

	revert.Add(func() { _ = os.Rename(backupSubvolume, target) })

	// Restore the snapshot.
	cleanup, err := d.snapshotSubvolume(srcVol.MountPath(), target, true)
	if err != nil {
		return err
	}

	if cleanup != nil {
		revert.Add(cleanup)
	}

	// Restore readonly property on subvolumes in reverse order (except root which should be left writable).
	subVolCount := len(subVols)
	for i := range subVols {
		i = subVolCount - 1 - i
		subVol := subVols[i]
		if subVol.Readonly && subVol.Path != string(filepath.Separator) {
			targetSubVolPath := filepath.Join(target, subVol.Path)
			err = d.setSubvolumeReadonlyProperty(targetSubVolPath, true)
			if err != nil {
				return err
			}
		}
	}

	revert.Success()

	// Remove the backup subvolume.
	return d.deleteSubvolume(backupSubvolume, true)
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *btrfs) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	return genericVFSRenameVolumeSnapshot(d, snapVol, newSnapshotName, op)
}
