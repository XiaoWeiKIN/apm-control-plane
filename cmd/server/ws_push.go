package main

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
)

// startWriter binds one asynchronous writer to the authenticated live WS session.
// The bounded wake channel coalesces multiple publishes; Desired remains in DB.
func (r *registry) startWriter(p auth.Principal, conn types.Connection, uid []byte, log *slog.Logger) {
	id := hex.EncodeToString(uid)
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	rt := r.sessions[key(p.WorkspaceID, id)]
	if rt == nil {
		r.mu.Unlock()
		return
	}
	rt.mu.Lock()
	if rt.conn != conn || rt.done != nil {
		rt.mu.Unlock()
		r.mu.Unlock()
		return
	}
	rt.wake = make(chan struct{}, 1)
	rt.done = make(chan struct{})
	wake, done := rt.wake, rt.done
	r.inFlight.Add(1)
	rt.mu.Unlock()
	r.mu.Unlock()

	go r.pushLoop(p.WorkspaceID, id, append([]byte(nil), uid...), conn, rt, wake, done, log)
	select {
	case wake <- struct{}{}:
	default:
	}
}

// wakeAgent is called only after the release transaction commits.
// It never blocks the Admin API on a database or network send.
func (r *registry) wakeAgent(workspaceID, uid string) {
	r.mu.Lock()
	rt := r.sessions[key(workspaceID, uid)]
	if rt == nil || r.closing {
		r.mu.Unlock()
		return
	}
	rt.mu.Lock()
	wake := rt.wake
	rt.mu.Unlock()
	r.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// pushLoop is the sole durable-config sender for one WS session.
// A periodic tick recovers lost wakeups and resends unacknowledged versions.
func (r *registry) pushLoop(workspaceID, uid string, rawUID []byte, conn types.Connection, rt *runtimeAgent, wake, done <-chan struct{}, log *slog.Logger) {
	defer r.inFlight.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-wake:
		case <-ticker.C:
		}
		select {
		case <-done:
			return
		default:
		}

		// 1. 原子读取当前 Desired 与重试期限，不能使用唤醒时的旧配置。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		desired, _, err := r.delivery.ReconcileAgentConfig(ctx, workspaceID, uid, nil, true)
		cancel()
		if err != nil {
			log.Warn("ws config reconciliation failed", "workspace_id", workspaceID, "instance_uid", uid, "error", err)
			continue
		}
		config, err := remoteConfigFromDesired(desired)
		if err != nil {
			log.Warn("ws config decode failed", "workspace_id", workspaceID, "instance_uid", uid, "error", err)
			continue
		}
		if config == nil {
			continue
		}

		// 2. 会话可能在 DB 查询时已关闭；不能向旧连接发送当前 Workspace 的配置。
		rt.mu.Lock()
		active := rt.conn == conn
		rt.mu.Unlock()
		if !active {
			return
		}
		select {
		case <-done:
			return
		default:
		}

		// 3. OpAMP 上游 WS Connection.Send 内部串行化物理写入。
		// 发送失败不回滚 Desired，下一次连接或重试从数据库恢复。
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		err = conn.Send(ctx, &protobufs.ServerToAgent{
			InstanceUid: rawUID,
			Capabilities: uint64(
				protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
					protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig),
			RemoteConfig: config,
		})
		cancel()
		if err != nil {
			log.Warn("ws remote config send failed", "workspace_id", workspaceID, "instance_uid", uid, "error", err)
			_ = conn.Disconnect()
			return
		}
	}
}
