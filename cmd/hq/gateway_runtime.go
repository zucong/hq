package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// sockaddr_un reserves one byte for the trailing NUL. Keep the direct
	// records/hq.sock endpoint when it fits; longer company roots use a
	// deterministic endpoint below a current-user-only runtime directory.
	gatewaySocketMaxBytes  = len(syscall.RawSockaddrUnix{}.Path) - 1
	gatewayHealthIOTimeout = 3 * time.Second
	// A first issue to an on_assignment seat can synchronously perform identity
	// and binding snapshots, CreateTab, a full Herdr cold start, its startup
	// prompt, another binding check, and the actual delivery prompt. The nominal
	// ceiling counts every normal-path external call. One additional full start
	// ceiling covers pane-readiness retry/recovery and ledger work; a request
	// context enforces this as the whole-dispatch ceiling rather than allowing
	// nested per-call timeouts to accumulate without bound.
	gatewayColdIssueNominalTimeout  = defaultHerdrStartTimeout + defaultHerdrMutationTimeout + 2*defaultHerdrPromptTimeout + 8*defaultHerdrSnapshotTimeout
	gatewayBusinessExecutionTimeout = gatewayColdIssueNominalTimeout + defaultHerdrStartTimeout
	// Keep a short response grace after dispatch cancellation so the server can
	// return an actionable, protocol-valid ambiguity response instead of causing
	// the client to infer another socket timeout.
	gatewayBusinessIOTimeout = gatewayBusinessExecutionTimeout + gatewayHealthIOTimeout
	gatewayMaxHandlers       = 32
)

type gatewayTimeoutPolicy struct {
	Health   time.Duration
	Business time.Duration
}

func defaultGatewayTimeoutPolicy() gatewayTimeoutPolicy {
	return gatewayTimeoutPolicy{Health: gatewayHealthIOTimeout, Business: gatewayBusinessIOTimeout}
}

func (p gatewayTimeoutPolicy) responseTimeout(request gatewayRequest) time.Duration {
	if request.Type == "request" {
		return p.Business
	}
	return p.Health
}

func (p gatewayTimeoutPolicy) valid() bool {
	return p.Health > 0 && p.Business > p.Health
}

func (p gatewayTimeoutPolicy) executionTimeout() time.Duration {
	return p.Business - p.Health
}

func gatewayDeadline(parent context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if parent != nil {
		if parent.Err() != nil {
			return time.Now()
		}
		if current, ok := parent.Deadline(); ok && current.Before(deadline) {
			return current
		}
	}
	return deadline
}

func gatewaySocketPath(dataDir string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("gateway 缺少 data directory")
	}
	direct := filepath.Join(dataDir, "hq.sock")
	if len([]byte(direct)) <= gatewaySocketMaxBytes {
		return direct, nil
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("解析 gateway data directory：%w", err)
	}
	digest := sha256.Sum256([]byte(filepath.Clean(abs)))
	runtimeDir := filepath.Join("/tmp", fmt.Sprintf("hq-gateway-%d", os.Getuid()))
	short := filepath.Join(runtimeDir, hex.EncodeToString(digest[:])+".sock")
	if len([]byte(short)) > gatewaySocketMaxBytes {
		return "", fmt.Errorf("gateway 短 socket path 仍超过操作系统上限（%d bytes > %d）：%s", len([]byte(short)), gatewaySocketMaxBytes, short)
	}
	return short, nil
}

func ensureGatewaySocketRuntimeDir(socket, dataDir string) error {
	direct := filepath.Join(dataDir, "hq.sock")
	if filepath.Clean(socket) == filepath.Clean(direct) {
		return nil
	}
	directory := filepath.Dir(socket)
	created := false
	if err := os.Mkdir(directory, 0o700); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("创建 gateway 私有 runtime 目录：%w", err)
		}
	} else {
		created = true
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("检查 gateway 私有 runtime 目录：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("gateway 私有 runtime 路径必须是非 symlink 目录：%s", directory)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("gateway 私有 runtime 目录 owner 不是当前 uid：%s", directory)
	}
	if created && info.Mode().Perm() != 0o700 {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("收紧 gateway 私有 runtime 目录权限：%w", err)
		}
		info, err = os.Lstat(directory)
		if err != nil {
			return fmt.Errorf("复核 gateway 私有 runtime 目录：%w", err)
		}
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("gateway 私有 runtime 目录 mode=%04o，要求 0700：%s", info.Mode().Perm(), directory)
	}
	return nil
}

type unixGatewayPinger struct{}

func (unixGatewayPinger) Ping(ctx context.Context, socket, expectedWorkspace string) GatewayHealth {
	health := GatewayHealth{}
	if expectedWorkspace == "" {
		health.Error = "缺少预期 workspace_id"
		return health
	}
	request := gatewayRequest{Version: gatewayProtocolVersion, Type: "ping", Workspace: expectedWorkspace}
	response, connected, err := exchangeGateway(ctx, socket, request)
	health.Connected = connected
	if err != nil {
		health.Error = err.Error()
		return health
	}
	health.Version, health.Workspace, health.ServerID = response.Version, response.Workspace, response.ServerID
	if !response.OK || response.Type != "pong" || response.Version != gatewayProtocolVersion || response.Workspace != expectedWorkspace || response.ServerID == "" {
		health.Error = "ping/pong 的版本、workspace 或 server identity 不匹配"
		return health
	}
	health.OK = true
	return health
}

func exchangeGateway(parent context.Context, socket string, request gatewayRequest) (gatewayResponse, bool, error) {
	return exchangeGatewayWithTimeouts(parent, socket, request, defaultGatewayTimeoutPolicy())
}

func exchangeGatewayWithTimeouts(parent context.Context, socket string, request gatewayRequest, policy gatewayTimeoutPolicy) (gatewayResponse, bool, error) {
	if parent == nil {
		parent = context.Background()
	}
	if !policy.valid() {
		return gatewayResponse{}, false, fmt.Errorf("gateway timeout policy 必须为正数")
	}
	// Dialing and request framing are local control-plane health operations and
	// must remain fail-fast. Only waiting for a valid business response receives
	// the cold-start-sized budget.
	ctx, cancel := context.WithTimeout(parent, policy.Health)
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	cancel()
	if err != nil {
		return gatewayResponse{}, false, err
	}
	defer conn.Close()
	stopCancellation := context.AfterFunc(parent, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancellation()
	if err := conn.SetWriteDeadline(gatewayDeadline(parent, policy.Health)); err != nil {
		return gatewayResponse{}, true, err
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return gatewayResponse{}, true, err
	}
	if unix, ok := conn.(*net.UnixConn); ok {
		if err := unix.CloseWrite(); err != nil {
			return gatewayResponse{}, true, err
		}
	}
	if err := conn.SetReadDeadline(gatewayDeadline(parent, policy.responseTimeout(request))); err != nil {
		return gatewayResponse{}, true, err
	}
	var response gatewayResponse
	if err := decodeSingleJSON(conn, gatewayRequestLimit, &response); err != nil {
		if parentErr := parent.Err(); parentErr != nil {
			return gatewayResponse{}, true, parentErr
		}
		return gatewayResponse{}, true, err
	}
	return response, true, nil
}

func (a *App) serveGatewayCommand(args []string) error {
	fs := newLeafParser("serve")
	fs.SetOutput(a.Err)
	workspaceID := fs.String("workspace-id", a.GatewayWorkspaceID, "绑定的 herdr workspace id")
	serverID := fs.String("server-id", a.GatewayServerID, "启动实例 identity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *workspaceID == "" {
		return fmt.Errorf("用法：hq --direct serve --workspace-id ID [--server-id ID]")
	}
	if *serverID == "" {
		generated, err := newGatewayServerID()
		if err != nil {
			return err
		}
		*serverID = generated
	}
	if _, err := gatewaySocketPath(a.DataDir); err != nil {
		return err
	}
	request := RuntimeAdmissionRequest{
		Action: runtimeAdmissionControlPlane,
		Target: a.Config.WorkspaceLabel,
	}
	// A read-only preflight preserves the zero-mutation denial contract for an
	// ESTOP that is already active. Its shared lease is released before
	// reconciliation; it must never be carried into an operation-lock wait.
	if _, err := a.withRuntimeAdmission(request, nil); err != nil {
		return err
	}
	// Reconcile takes the per-delivery operation lock before any Runtime
	// Admission lease. Do not wrap it in the gateway's outer control-plane
	// lease: that would invert the global operation -> ESTOP order and can
	// deadlock with a live delivery plus a waiting ESTOP activation.
	if err := a.reconcileDeliveries(); err != nil {
		return fmt.Errorf("网关启动前 outbox reconcile：%w", err)
	}
	var prepared preparedGateway
	_, err := a.withRuntimeAdmission(request, func() error {
		var prepareErr error
		prepared, prepareErr = a.prepareGatewayAdmitted(*workspaceID)
		return prepareErr
	})
	if err != nil {
		return err
	}
	return a.runPreparedGateway(prepared, *workspaceID, *serverID)
}

type preparedGateway struct {
	listener net.Listener
	socket   string
	identity gatewaySocketIdentity
}

// prepareGatewayAdmitted performs the socket startup mutations while the
// caller holds the Runtime Admission shared lease. Delivery reconciliation is
// deliberately completed before this function so its operation -> ESTOP lock
// order cannot be inverted. The lease must not cover the long-running accept
// loop: ESTOP activation needs the exclusive lease while the gateway remains
// online so operators can freeze and later release HQ.
func (a *App) prepareGatewayAdmitted(workspaceID string) (preparedGateway, error) {
	socket, err := gatewaySocketPath(a.DataDir)
	if err != nil {
		return preparedGateway{}, err
	}
	if err := os.MkdirAll(a.DataDir, 0o755); err != nil {
		return preparedGateway{}, err
	}
	if a.ProductionRuntime {
		if err := validateProductionRuntime(runtimePaths{
			Office: a.Office, HQRoot: a.HQRoot, DataDir: a.DataDir,
			ConfigPath: a.ConfigPath, HerdrBin: a.HerdrBin,
		}); err != nil {
			return preparedGateway{}, fmt.Errorf("创建 data 后复核正式根：%w", err)
		}
	}
	if err := ensureGatewaySocketRuntimeDir(socket, a.DataDir); err != nil {
		return preparedGateway{}, err
	}
	startupLock, err := lockGatewayStartup(filepath.Join(a.DataDir, ".gateway-start.lock"))
	if err != nil {
		return preparedGateway{}, err
	}
	locked := true
	defer func() {
		if locked {
			unlock(startupLock)
		}
	}()
	if exists, err := validateExistingGatewaySocket(socket); err != nil {
		return preparedGateway{}, err
	} else if exists {
		first := (unixGatewayPinger{}).Ping(context.Background(), socket, workspaceID)
		time.Sleep(100 * time.Millisecond)
		second := (unixGatewayPinger{}).Ping(context.Background(), socket, workspaceID)
		if first.OK || second.OK {
			return preparedGateway{}, fmt.Errorf("HQ 网关已经在运行：%s", socket)
		}
		if first.Connected || second.Connected {
			return preparedGateway{}, fmt.Errorf("现有 socket 有 listener 但未通过 HQ 协议身份核验，拒绝删除：%s", socket)
		}
		if err := os.Remove(socket); err != nil {
			return preparedGateway{}, fmt.Errorf("两次协议复核均确认无 listener 后清理 stale socket：%w", err)
		}
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		return preparedGateway{}, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return preparedGateway{}, err
	}
	identity, err := socketFileIdentity(socket)
	if err != nil {
		listener.Close()
		return preparedGateway{}, err
	}
	unlock(startupLock)
	locked = false
	return preparedGateway{listener: listener, socket: socket, identity: identity}, nil
}

func (a *App) runPreparedGateway(prepared preparedGateway, workspaceID, serverID string) error {
	ctx := a.GatewayContext
	var cancel context.CancelFunc
	if ctx == nil {
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
	}
	fmt.Fprintf(a.Out, "[HQ system] HQ 网关已启动：%s workspace=%s server=%s\n", prepared.socket, workspaceID, serverID)
	err := a.serveGateway(ctx, prepared.listener, workspaceID, serverID)
	_ = prepared.listener.Close()
	if cleanupErr := removeOwnedSocket(prepared.socket, prepared.identity); cleanupErr != nil && err == nil {
		err = cleanupErr
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type gatewaySynchronizedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w gatewaySynchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func (a *App) serveGateway(ctx context.Context, listener net.Listener, workspaceID, serverID string) error {
	// Request handlers and both background workers may log concurrently.
	child := *a
	outputMu := &sync.Mutex{}
	child.Out = gatewaySynchronizedWriter{outputMu, a.Out}
	child.Err = gatewaySynchronizedWriter{outputMu, a.Err}
	a = &child
	semaphore := make(chan struct{}, gatewayMaxHandlers)
	var handlers sync.WaitGroup
	defer handlers.Wait()
	var reaper sync.WaitGroup
	if a.RuntimeReaperInterval > 0 && a.Herdr != nil && a.Sessions != nil && a.Store != nil {
		reaperCtx, cancelReaper := context.WithCancel(ctx)
		if a.ConfigPath != "" {
			reaper.Add(1)
			go func() { defer reaper.Done(); a.runConfigWatcher(reaperCtx) }()
		}
		reaper.Add(1)
		go func() {
			defer reaper.Done()
			a.runRuntimeReaperOnce(reaperCtx)
			ticker := time.NewTicker(a.RuntimeReaperInterval)
			defer ticker.Stop()
			for {
				select {
				case <-reaperCtx.Done():
					return
				case <-ticker.C:
					a.runRuntimeReaperOnce(reaperCtx)
				}
			}
		}()
		defer func() {
			cancelReaper()
			reaper.Wait()
		}()
	}
	for {
		if unix, ok := listener.(*net.UnixListener); ok {
			_ = unix.SetDeadline(time.Now().Add(250 * time.Millisecond))
		}
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}
		select {
		case semaphore <- struct{}{}:
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-semaphore }()
				a.handleGatewayConnectionContext(ctx, conn, workspaceID, serverID, defaultGatewayTimeoutPolicy())
			}()
		default:
			_ = conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
			response := gatewayResponse{Version: gatewayProtocolVersion, Type: "response", Workspace: workspaceID, ServerID: serverID, Error: "gateway handler 并发上限已满"}
			if err := json.NewEncoder(conn).Encode(response); err != nil {
				fmt.Fprintf(a.Err, "gateway 并发拒绝响应写失败：%v\n", err)
			}
			_ = conn.Close()
		}
	}
}

func (a *App) handleGatewayConnection(conn net.Conn, workspaceID, serverID string) {
	a.handleGatewayConnectionContext(context.Background(), conn, workspaceID, serverID, defaultGatewayTimeoutPolicy())
}

func (a *App) handleGatewayConnectionWithTimeouts(conn net.Conn, workspaceID, serverID string, policy gatewayTimeoutPolicy) {
	a.handleGatewayConnectionContext(context.Background(), conn, workspaceID, serverID, policy)
}

func (a *App) handleGatewayConnectionContext(parent context.Context, conn net.Conn, workspaceID, serverID string, policy gatewayTimeoutPolicy) {
	defer conn.Close()
	if parent == nil {
		parent = context.Background()
	}
	// serveGateway waits for every handler before returning. A business socket
	// can otherwise retain its long cold-start deadline after server shutdown,
	// so interrupt both reads and writes as soon as the serve context is
	// canceled. Dispatch receives the same parent below and is canceled too.
	stopParentIOInterrupt := context.AfterFunc(parent, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopParentIOInterrupt()
	if !policy.valid() {
		fmt.Fprintln(a.Err, "gateway timeout policy 必须为正数")
		return
	}
	if err := conn.SetDeadline(gatewayDeadline(parent, policy.Health)); err != nil {
		fmt.Fprintf(a.Err, "gateway 设置连接 deadline 失败：%v\n", err)
		return
	}
	response := gatewayResponse{Version: gatewayProtocolVersion, Type: "response", Workspace: workspaceID, ServerID: serverID}
	var request gatewayRequest
	if err := decodeSingleJSON(conn, gatewayRequestLimit, &request); err != nil {
		response.Error = "无效网关请求：" + err.Error()
		a.writeGatewayResponse(conn, response)
		return
	}
	if request.Version != gatewayProtocolVersion || request.Workspace != workspaceID {
		response.Error = "网关请求的协议版本或 workspace 不匹配"
		a.writeGatewayResponse(conn, response)
		return
	}
	if request.Type == "ping" {
		response.Type, response.OK = "pong", true
		a.writeGatewayResponse(conn, response)
		return
	}
	if request.Type != "request" || request.PaneID == "" || len(request.Args) == 0 {
		response.Error = "网关请求缺少 type/pane_id/命令"
		a.writeGatewayResponse(conn, response)
		return
	}
	if !shouldUseGateway(request.Args) {
		response.Error = "该命令不允许通过网关执行"
		a.writeGatewayResponse(conn, response)
		return
	}
	// The framing deadline has served its purpose. Dispatch may synchronously
	// cold-start an on_assignment seat and then deliver the prompt, so both the
	// server write and client read must use the same longer business budget.
	if err := conn.SetDeadline(gatewayDeadline(parent, policy.Business)); err != nil {
		fmt.Fprintf(a.Err, "gateway 设置业务 deadline 失败：%v\n", err)
		return
	}
	executionCtx, cancelExecution := context.WithTimeout(parent, policy.executionTimeout())
	defer cancelExecution()
	recoveryCtx, cancelRecovery := context.WithTimeout(parent, policy.Business)
	defer cancelRecovery()
	unlockConfig, lockErr := a.lockGatewayConfigAccessContext(executionCtx, request.Args)
	if lockErr != nil {
		response.Error = "gateway 业务执行预算已到期且尚未取得配置执行权；请求未 dispatch，可在网关恢复后重试：" + lockErr.Error()
		a.writeGatewayResponse(conn, response)
		return
	}
	defer unlockConfig()
	var stdout, stderr bytes.Buffer
	child := *a
	cfg, err := loadConfig(a.ConfigPath)
	if err != nil {
		response.Error = "重新加载 HQ 配置：" + err.Error()
		a.writeGatewayResponse(conn, response)
		return
	}
	child.Config = cfg
	child.Out, child.Err = &stdout, &stderr
	child.RequestContext = executionCtx
	if store, ok := a.Store.(*Store); ok {
		child.Store = store.withRequestContext(executionCtx)
		child.RecoveryStore = store.withRequestContext(recoveryCtx)
	}
	if sessions, ok := a.Sessions.(*FileSessionStore); ok {
		child.Sessions = sessions.withRequestContext(executionCtx)
	}
	child.CallerPane, child.FromGateway = request.PaneID, true
	child.Direct, child.MaintenancePane = false, ""
	child.DryRun, child.JSON = request.DryRun, request.JSON
	if contextErr := executionCtx.Err(); contextErr != nil {
		response.Error = "gateway 业务执行预算已到期且尚未 dispatch；可在网关恢复后重试：" + contextErr.Error()
		a.writeGatewayResponse(conn, response)
		return
	}
	err = child.dispatch(request.Args)
	response.Stdout, response.Stderr = stdout.String(), stderr.String()
	if executionCtx.Err() != nil && err != nil {
		response.Error = fmt.Sprintf("gateway 业务执行预算到期：%v；结果可能已部分落账或投递，禁止直接重复执行。先运行只读核验：%s；原始错误：%v", executionCtx.Err(), gatewayReadOnlyRecovery(request.Args), err)
	} else if err != nil {
		response.Error = gatewayDispatchError(request.Args, err)
	} else {
		response.OK = true
	}
	a.writeGatewayResponse(conn, response)
}

func gatewayDispatchError(args []string, err error) string {
	var outcomeErr *DeliveryOutcomeError
	if !errors.As(err, &outcomeErr) ||
		(outcomeErr.Outcome.DeliveryStatus != deliveryUnknown && outcomeErr.Outcome.DeliveryStatus != deliveryAttempted) {
		return err.Error()
	}
	return fmt.Sprintf("%v；结果不确定，命令可能已经落账或投递，禁止直接重复执行。先运行只读核验：%s；再按 delivery 的 next_action 恢复", err, gatewayReadOnlyRecovery(args))
}

func isRegistryConfigMutation(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[0] {
	case "staff":
		switch args[1] {
		case "add", "update", "remove":
			return true
		}
	case "role":
		switch args[1] {
		case "add", "retire":
			return true
		}
	}
	return false
}

// lockGatewayConfigAccess prevents a request from loading one registry version
// and appending under it after a concurrent registry mutation has installed a new
// version. Child App copies share the pointer initialized by the gateway App.
func (a *App) lockGatewayConfigAccess(args []string) func() {
	if a.ConfigAccess == nil {
		return func() {}
	}
	if isRegistryConfigMutation(args) {
		a.ConfigAccess.Lock()
		return a.ConfigAccess.Unlock
	}
	a.ConfigAccess.RLock()
	return a.ConfigAccess.RUnlock
}

func (a *App) lockGatewayConfigAccessContext(ctx context.Context, args []string) (func(), error) {
	if a.ConfigAccess == nil {
		return func() {}, nil
	}
	if isRegistryConfigMutation(args) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A polling TryLock never registers a waiting writer with RWMutex.
		// Queue the writer so the next maintenance RLock cannot overtake it.
		// The unbuffered handoff also releases a lock acquired after cancellation.
		acquired := make(chan struct{})
		go func() {
			a.ConfigAccess.Lock()
			select {
			case acquired <- struct{}{}:
			case <-ctx.Done():
				a.ConfigAccess.Unlock()
			}
		}()
		select {
		case <-acquired:
			if err := ctx.Err(); err != nil {
				a.ConfigAccess.Unlock()
				return nil, err
			}
			return a.ConfigAccess.Unlock, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	try := a.ConfigAccess.TryRLock
	unlock := a.ConfigAccess.RUnlock
	acquire := func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !try() {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return false, err
		}
		return true, nil
	}
	if acquired, err := acquire(); acquired {
		return unlock, nil
	} else if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if acquired, err := acquire(); acquired {
				return unlock, nil
			} else if err != nil {
				return nil, err
			}
		}
	}
}

func (a *App) writeGatewayResponse(conn net.Conn, response gatewayResponse) {
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		fmt.Fprintf(a.Err, "gateway 响应写失败：%v\n", err)
	}
}

func decodeSingleJSON(reader io.Reader, limit int64, target any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("消息超过 %d bytes", limit)
	}
	return decodeStrictJSON(raw, target)
}

func lockGatewayStartup(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("gateway 启动锁必须是普通文件：%s", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func validateExistingGatewaySocket(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, fmt.Errorf("gateway path 已存在但不是 socket，拒绝删除：%s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return false, fmt.Errorf("gateway socket owner 不是当前 uid，拒绝使用：%s", path)
	}
	if info.Mode().Perm() != 0o600 {
		return false, fmt.Errorf("gateway socket mode=%04o，要求 0600：%s", info.Mode().Perm(), path)
	}
	return true, nil
}

type gatewaySocketIdentity struct {
	Dev uint64
	Ino uint64
}

func socketFileIdentity(path string) (gatewaySocketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return gatewaySocketIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return gatewaySocketIdentity{}, fmt.Errorf("无法读取 socket identity：%s", path)
	}
	return gatewaySocketIdentity{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil
}

func removeOwnedSocket(path string, expected gatewaySocketIdentity) error {
	actual, err := socketFileIdentity(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("退出清理发现 socket identity 已变化，拒绝删除对方 socket：%s", path)
	}
	return os.Remove(path)
}

func newGatewayServerID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "gateway-" + strings.ToLower(hex.EncodeToString(raw)), nil
}
