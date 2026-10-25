package executors

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/sealstream"
)

// validVolumeFileName gates names embedded in restore scripts.
var volumeFileNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validVolumeFileName(name string) error {
	if !volumeFileNameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid volume file name %q", name)
	}
	return nil
}

// validObjectKey rejects path traversal: payload contents are never trusted.
func validObjectKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return fmt.Errorf("invalid object key %q", key)
	}
	return nil
}

// s3Client goes direct, or through the gateway for a relayed backup buddy.
func (d *Deployer) s3Client(t *agentv1.S3Target) (*minio.Client, error) {
	endpoint := t.GetEndpoint()
	if t.GetRelayToken() != "" {
		endpoint = d.controlPlaneURL
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid s3 endpoint")
	}
	opts := &minio.Options{
		Creds:        credentials.NewStaticV4(t.GetAccessKeyId(), t.GetSecretAccessKey(), ""),
		Secure:       u.Scheme == "https",
		Region:       t.GetRegion(),
		BucketLookup: lookupStyle(t.GetForcePathStyle()),
	}
	if tok := t.GetRelayToken(); tok != "" {
		base, err := minio.DefaultTransport(opts.Secure)
		if err != nil {
			return nil, err
		}
		opts.Transport = relayRoundTripper{base: base, token: tok}
		// The gateway serves the bucket at its root, so only path-style works.
		opts.BucketLookup = minio.BucketLookupPath
	}
	return minio.New(u.Host, opts)
}

func lookupStyle(pathStyle bool) minio.BucketLookupType {
	if pathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupDNS
}

// Backup streams straight to the user's S3: no staging file, nothing via the control plane.
func (d *Deployer) Backup(ctx context.Context, cmd *agentv1.Backup, emit Emit) error {
	result := func(ok bool, size uint64, errMsg string) error {
		return emit(&agentv1.Event{Payload: &agentv1.Event_BackupResult{BackupResult: &agentv1.BackupResult{
			BackupId: cmd.GetBackupId(), Kind: "backup", Ok: ok, SizeBytes: size, Error: errMsg,
		}}})
	}
	size, err := d.runBackup(ctx, cmd)
	if err != nil {
		return result(false, 0, err.Error())
	}
	return result(true, size, "")
}

func (d *Deployer) runBackup(ctx context.Context, cmd *agentv1.Backup) (uint64, error) {
	if err := validObjectKey(cmd.GetObjectKey()); err != nil {
		return 0, err
	}
	client, err := d.s3Client(cmd.GetS3())
	if err != nil {
		return 0, err
	}
	if err := ensureBucket(ctx, client, cmd.GetS3()); err != nil {
		return 0, err
	}
	source, err := d.backupSourceCmd(ctx, cmd)
	if err != nil {
		return 0, err
	}
	stdout, err := source.StdoutPipe()
	if err != nil {
		return 0, err
	}
	var stderr strings.Builder
	source.Stderr = &stderr
	if err := source.Start(); err != nil {
		return 0, err
	}

	// Encrypted before it leaves, so the bucket only sees ciphertext. Commands without a key
	// upload as before.
	var upload io.Reader = stdout
	var sealErr chan error
	var sealed *io.PipeReader
	if key := cmd.GetEncryptionKey(); len(key) > 0 {
		pr, pw := io.Pipe()
		sealed = pr
		sealErr = make(chan error, 1)
		go func() {
			w, err := sealstream.NewWriter(pw, key)
			if err == nil {
				_, err = io.Copy(w, stdout)
				if cerr := w.Close(); err == nil {
					err = cerr
				}
			}
			sealErr <- err
			_ = pw.CloseWithError(err)
		}()
		upload = pr
	}

	// Streamed multipart upload; parts are buffered in memory, so keep them small.
	info, putErr := client.PutObject(ctx, cmd.GetS3().GetBucket(), cmd.GetObjectKey(), upload, -1,
		minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: 16 * 1024 * 1024})
	if putErr == nil && sealErr != nil {
		putErr = <-sealErr
	}
	if putErr != nil && sealed != nil {
		// Unblocks the sealing goroutine.
		_ = sealed.CloseWithError(putErr)
	}
	if putErr != nil {
		// Kill the source before Wait so a failed upload can't block until the timeout.
		_ = source.Process.Kill()
		_ = source.Wait()
		return 0, fmt.Errorf("s3 upload: %w", putErr)
	}
	waitErr := source.Wait()
	if waitErr != nil {
		// The upload finished but the source failed, so the object is incomplete.
		_ = client.RemoveObject(context.Background(), cmd.GetS3().GetBucket(), cmd.GetObjectKey(), minio.RemoveObjectOptions{})
		return 0, fmt.Errorf("backup source: %s", firstLine(stderr.String(), waitErr))
	}
	return uint64(info.Size), nil
}

func (d *Deployer) backupSourceCmd(ctx context.Context, cmd *agentv1.Backup) (*exec.Cmd, error) {
	if cmd.GetVolumeName() != "" {
		return d.orch.VolumeTarCmd(ctx, cmd.GetWorkloadId(), cmd.GetVolumeName())
	}
	engine, ok := engineBackups[cmd.GetService().GetEngine()]
	if !ok || engine.dumpScript == "" {
		return nil, fmt.Errorf("no dump support for engine %q", cmd.GetService().GetEngine())
	}
	return d.orch.AppExecCmd(ctx, cmd.GetWorkloadId(), engine.dumpScript)
}

// RestoreBackup: volume and redis restores stop the workload around the write; engine
// restores replay the dump through the running service.
func (d *Deployer) RestoreBackup(ctx context.Context, cmd *agentv1.RestoreBackup, emit Emit) error {
	result := func(ok bool, errMsg string) error {
		return emit(&agentv1.Event{Payload: &agentv1.Event_BackupResult{BackupResult: &agentv1.BackupResult{
			BackupId: cmd.GetBackupId(), Kind: "restore", Ok: ok, Error: errMsg,
		}}})
	}
	if err := d.runRestore(ctx, cmd); err != nil {
		return result(false, err.Error())
	}
	return result(true, "")
}

func (d *Deployer) runRestore(ctx context.Context, cmd *agentv1.RestoreBackup) error {
	if err := validObjectKey(cmd.GetObjectKey()); err != nil {
		return err
	}
	client, err := d.s3Client(cmd.GetS3())
	if err != nil {
		return err
	}
	object, err := client.GetObject(ctx, cmd.GetS3().GetBucket(), cmd.GetObjectKey(), minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 download: %w", err)
	}
	defer object.Close()

	wl := cmd.GetWorkloadId()
	engine, isService := engineBackups[cmd.GetService().GetEngine()]

	// File-based restores replace data under the engine, so stop it first.
	stopped := false
	if cmd.GetVolumeName() != "" || (isService && engine.restoreVolumeFile != "") {
		// A planned stop, not an outage: the watch restarts clean once the workload is back.
		d.appmon.Pause(wl)
		if err := d.orch.Stop(ctx, wl); err != nil {
			d.appmon.Resume(wl)
			return fmt.Errorf("stop workload: %w", err)
		}
		stopped = true
		defer func() {
			startCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = d.orch.Start(startCtx, wl)
			d.appmon.Resume(wl)
		}()
	}

	sink, err := d.restoreSinkCmd(ctx, cmd, engine, isService)
	if err != nil {
		return err
	}
	data, err := opened(object, cmd.GetEncryptionKey())
	if err != nil {
		return err
	}
	sink.Stdin = data
	if out, err := sink.CombinedOutput(); err != nil {
		return fmt.Errorf("restore: %s", firstLine(string(out), err))
	}
	_ = stopped // the deferred Start covers every stopped path
	return nil
}

func (d *Deployer) restoreSinkCmd(ctx context.Context, cmd *agentv1.RestoreBackup, engine engineBackup, isService bool) (*exec.Cmd, error) {
	if cmd.GetVolumeName() != "" {
		return d.orch.VolumeUntarCmd(ctx, cmd.GetWorkloadId(), cmd.GetVolumeName())
	}
	if !isService {
		return nil, fmt.Errorf("no restore support for engine %q", cmd.GetService().GetEngine())
	}
	if engine.restoreVolumeFile != "" {
		// Managed services always name their data volume 'data'.
		return d.orch.VolumeWriteFileCmd(ctx, cmd.GetWorkloadId(), "data", engine.restoreVolumeFile)
	}
	return d.orch.AppExecCmd(ctx, cmd.GetWorkloadId(), engine.restoreScript)
}

// DeleteBackup removes the object; the control plane deletes the row only after success.
func (d *Deployer) DeleteBackup(ctx context.Context, cmd *agentv1.DeleteBackup, emit Emit) error {
	result := func(ok bool, errMsg string) error {
		return emit(&agentv1.Event{Payload: &agentv1.Event_BackupResult{BackupResult: &agentv1.BackupResult{
			BackupId: cmd.GetBackupId(), Kind: "delete", Ok: ok, Error: errMsg,
		}}})
	}
	if err := validObjectKey(cmd.GetObjectKey()); err != nil {
		return result(false, err.Error())
	}
	client, err := d.s3Client(cmd.GetS3())
	if err != nil {
		return result(false, err.Error())
	}
	if err := client.RemoveObject(ctx, cmd.GetS3().GetBucket(), cmd.GetObjectKey(), minio.RemoveObjectOptions{}); err != nil {
		return result(false, err.Error())
	}
	return result(true, "")
}

// restoreVolumesFromHints restores migration backups before the container first starts.
func (d *Deployer) restoreVolumesFromHints(ctx context.Context, workloadID string, hints []*agentv1.RestoreHint, logLine LogFunc) error {
	for _, hint := range hints {
		if err := validObjectKey(hint.GetObjectKey()); err != nil {
			return err
		}
		client, err := d.s3Client(hint.GetS3())
		if err != nil {
			return err
		}
		object, err := client.GetObject(ctx, hint.GetS3().GetBucket(), hint.GetObjectKey(), minio.GetObjectOptions{})
		if err != nil {
			return fmt.Errorf("s3 download: %w", err)
		}
		sink, err := d.orch.VolumeUntarCmd(ctx, workloadID, hint.GetVolumeName())
		if err != nil {
			object.Close()
			return err
		}
		data, err := opened(object, hint.GetEncryptionKey())
		if err != nil {
			object.Close()
			return err
		}
		sink.Stdin = data
		out, err := sink.CombinedOutput()
		object.Close()
		if err != nil {
			return fmt.Errorf("restore volume %s: %s", hint.GetVolumeName(), firstLine(string(out), err))
		}
		logLine(agentv1.LogStream_LOG_STREAM_SYSTEM_OK, "restored volume "+hint.GetVolumeName()+" from backup")
	}
	return nil
}

func firstLine(out string, fallback error) string {
	if line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]); line != "" {
		return line
	}
	return fallback.Error()
}

// ensureBucket creates a backup buddy's bucket on first use; other targets are the user's.
func ensureBucket(ctx context.Context, client *minio.Client, t *agentv1.S3Target) error {
	if !t.GetEnsureBucket() {
		return nil
	}
	exists, err := client.BucketExists(ctx, t.GetBucket())
	if err != nil {
		return fmt.Errorf("backup buddy unreachable: %w", err)
	}
	if exists {
		return nil
	}
	return client.MakeBucket(ctx, t.GetBucket(), minio.MakeBucketOptions{Region: t.GetRegion()})
}

// opened decrypts a sealed backup as it streams; a tampered stream fails the restore.
func opened(object io.Reader, key []byte) (io.Reader, error) {
	if len(key) == 0 {
		return object, nil
	}
	r, err := sealstream.NewReader(object, key)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	return r, nil
}
