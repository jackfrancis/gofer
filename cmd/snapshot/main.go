// Command snapshot takes one CSI VolumeSnapshot of gofer's data volume, waits until
// it is usable, and prunes old ones and failed ones (internal/snapshot). The backup CronJob
// (deploy/k8s/backup) runs it in-cluster from the web image.
//
// Configuration (environment):
//
//	SNAPSHOT_CLASS  the VolumeSnapshotClass to use (required)
//	SNAPSHOT_CLAIM  the claim to snapshot (default data-gofer-0)
//	SNAPSHOT_KEEP   usable snapshots to keep (default 28)
//	SNAPSHOT_WAIT   how long to wait for the snapshot to be usable (default 10m); also
//	                how long any snapshot may keep reporting an error before it is
//	                deleted as failed
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackfrancis/gofer/internal/snapshot"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("backup failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	keep, err := strconv.Atoi(getenv("SNAPSHOT_KEEP", "28"))
	if err != nil || keep < 1 {
		return fmt.Errorf("SNAPSHOT_KEEP must be a positive integer, got %q", os.Getenv("SNAPSHOT_KEEP"))
	}
	wait, err := time.ParseDuration(getenv("SNAPSHOT_WAIT", "10m"))
	if err != nil || wait <= 0 {
		return fmt.Errorf("SNAPSHOT_WAIT must be a positive duration, got %q", os.Getenv("SNAPSHOT_WAIT"))
	}
	class := os.Getenv("SNAPSHOT_CLASS")
	if class == "" {
		return errors.New("SNAPSHOT_CLASS must name a VolumeSnapshotClass")
	}
	kube, ns, err := snapshot.InCluster()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	s := &snapshot.Snapshotter{
		Kube:      kube,
		Namespace: ns,
		Claim:     getenv("SNAPSHOT_CLAIM", "data-gofer-0"),
		Class:     class,
		Keep:      keep,
		Grace:     wait, // an erroring snapshot gets as long to recover as a new one gets to finish
		Log:       log,
	}
	return s.Run(ctx, wait)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
