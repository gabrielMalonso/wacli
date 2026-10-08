package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

const syncStatusKind = "sync_status_v1"

type syncStatusRequest struct {
	ID       string `json:"id"`
	StoreRef string `json:"store_ref"`
}
type syncStatusReply struct {
	Version  int                `json:"version"`
	ID       string             `json:"id"`
	StoreRef string             `json:"store_ref"`
	Status   app.SyncLiveStatus `json:"status"`
}
type syncStatusData struct {
	app.SyncLiveStatus
	Reason string `json:"reason,omitempty"`
}

// The early follow socket serves status during bootstrap. Mutation requests
// retain their existing dispatch path only after local initialization finishes.
func startSyncDelegateServer(ctx context.Context, a *app.App, spacing sendSpacing) (func(), error) {
	return startSendDelegateServerForStore(context.WithoutCancel(ctx), a.StoreDir(), spacing, func(requestCtx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		status := a.SyncLiveSnapshot()
		if req.Kind == syncStatusKind {
			ref, err := filepath.Abs(a.StoreDir())
			if err != nil || req.Version != sendDelegateVersion || req.SyncStatus == nil || req.SyncStatus.ID == "" || req.SyncStatus.StoreRef != ref {
				return sendDelegateResponse{OK: false, Error: "sync status scope refused"}, nil
			}
			return sendDelegateResponse{OK: true, SyncStatus: &syncStatusReply{Version: 1, ID: req.SyncStatus.ID, StoreRef: ref, Status: status}}, nil
		}
		if !status.Initialized || status.State == "stopping" || status.State == "stopped" || status.State == "logged_out" || status.State == "error" {
			return sendDelegateResponse{OK: false, Error: "sync owner is initializing or stopping; request was not dispatched"}, nil
		}
		// Keep the original Sync cancellation budget for mutations while status
		// remains observable during cleanup until the CLI explicitly stops IPC.
		actionCtx, cancel := context.WithCancel(requestCtx)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		if ctx.Err() != nil {
			cancel()
		}
		return executeDelegatedSend(actionCtx, a, req)
	})
}

func newSyncStatusCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Query the existing follow owner's live readiness without connecting",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("timeout") && (flags.timeout <= 0 || flags.timeout > time.Minute) {
				return agentUsageError(errors.New("sync status --timeout must be positive and at most 1m"))
			}
			dir, err := resolveStoreDir(flags)
			if err != nil {
				return agentStoreError(err)
			}
			// Five seconds by default; explicit --timeout and cancellation bound all IPC.
			budget := min(flags.timeout, 5*time.Second)
			if cmd.Flags().Changed("timeout") {
				budget = flags.timeout
			}
			signalCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(signalCtx, budget)
			defer cancel()
			status := querySyncStatus(ctx, dir)
			if flags.agent {
				return out.WriteAgentJSON(os.Stdout, flags.agentAccount, agentMeta(flags), status)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, status)
			}
			tw := newTableWriter(os.Stdout)
			fmt.Fprintln(tw, "STATE\tREADY\tCONNECTED\tREASON")
			fmt.Fprintf(tw, "%s\t%t\t%s\t%s\n", status.State, status.Ready, status.Connected, status.Reason)
			return tw.Flush()
		},
	}
}

func querySyncStatus(ctx context.Context, storeRef string) syncStatusData {
	unknown := func(reason string) syncStatusData {
		return syncStatusData{SyncLiveStatus: app.SyncLiveStatus{State: "unknown", Connected: "unknown", ObservedAt: time.Now().UTC()}, Reason: reason}
	}
	absent := func() syncStatusData { v := unknown("owner_absent"); v.State = "absent"; return v }
	if ctx.Err() != nil {
		return unknown(syncStatusContextReason(ctx))
	}
	if runtime.GOOS == "windows" {
		return unknown("ipc_unsupported")
	}
	ref, err := filepath.Abs(storeRef)
	if err != nil {
		return unknown("scope_unavailable")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sendDelegateSocketPath(ref))
	if err != nil {
		if ctx.Err() != nil {
			return unknown(syncStatusContextReason(ctx))
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return absent()
		}
		return unknown("ipc_unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	id := rand.Text()
	req := sendDelegateRequest{Version: sendDelegateVersion, Kind: syncStatusKind, SyncStatus: &syncStatusRequest{ID: id, StoreRef: ref}}
	if deadline, ok := ctx.Deadline(); ok {
		req.DeadlineUnixMS = deadline.UnixMilli()
		req.TimeoutMS = max(1, time.Until(deadline).Milliseconds())
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		if ctx.Err() != nil {
			return unknown(syncStatusContextReason(ctx))
		}
		return unknown("ipc_unavailable")
	}
	var resp sendDelegateResponse
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return unknown(syncStatusContextReason(ctx))
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return unknown("timeout")
		}
		return unknown("invalid_reply")
	}
	if ctx.Err() != nil {
		return unknown(syncStatusContextReason(ctx))
	}
	reply := resp.SyncStatus
	if !resp.OK || reply == nil || reply.Version != 1 {
		return unknown("owner_incompatible")
	}
	if reply.ID != id || reply.StoreRef != ref {
		return unknown("scope_mismatch")
	}
	if !validSyncLiveStatus(reply.Status) {
		return unknown("invalid_reply")
	}
	return syncStatusData{SyncLiveStatus: reply.Status}
}

func syncStatusContextReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancelled"
}

func validSyncLiveStatus(v app.SyncLiveStatus) bool {
	if v.ObservedAt.IsZero() || v.ObservedAt.After(time.Now().Add(time.Second)) || time.Since(v.ObservedAt) > time.Minute {
		return false
	}
	switch v.State {
	case "initializing", "ready", "disconnected", "reconnecting", "stopping", "stopped", "logged_out", "error", "unknown":
	default:
		return false
	}
	if v.Connected != "true" && v.Connected != "false" && v.Connected != "unknown" {
		return false
	}
	if v.Ready != (v.State == "ready") || v.Ready && (!v.Initialized || v.Connected != "true") {
		return false
	}
	if v.Connected == "true" && v.State != "initializing" && v.State != "ready" && v.State != "stopping" && v.State != "stopped" && v.State != "error" {
		return false
	}
	if v.LinkedLID != "" {
		lid, err := types.ParseJID(v.LinkedLID)
		if err != nil || lid.IsEmpty() || lid.Server != types.HiddenUserServer {
			return false
		}
	}
	if v.Ready {
		jid, err := types.ParseJID(v.LinkedJID)
		if err != nil || jid.IsEmpty() || jid.Server != types.DefaultUserServer {
			return false
		}
	}
	return true
}
