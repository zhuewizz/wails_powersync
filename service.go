package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	powersync "github.com/zhuewizz/powersync_golang"
	"github.com/zhuewizz/powersync_golang/examples/cutterschema"
	"github.com/zhuewizz/powersync_golang/restconn"
)

// beijingZone 是固定 +8 时区（北京时间）；用 FixedZone 避免依赖系统 tzdata。
var beijingZone = time.FixedZone("CST", 8*3600)

// aliasTables 是同步规则里用短别名（AS i 等）的表：服务端下发的 object_type 就是别名。
var aliasTables = map[string]bool{"d": true, "h": true, "i": true, "l": true, "r": true, "s": true, "t": true}

// Config 是服务参数（默认值写死在本机测试环境，可用环境变量覆盖）。
type Config struct {
	DBPath   string `json:"dbPath,omitempty"`
	Endpoint string `json:"endpoint"`
	RPC      string `json:"rpc"`
	Box      string `json:"box"`
	Org      string `json:"org"`
	REST     string `json:"rest,omitempty"`
}

// IsZero 判断连接参数是否齐全（缺任何一项都需要在界面里填）。
func (c Config) IsZero() bool {
	return strings.TrimSpace(c.Endpoint) == "" ||
		strings.TrimSpace(c.RPC) == "" ||
		strings.TrimSpace(c.Box) == "" ||
		strings.TrimSpace(c.Org) == ""
}

// configPath 是连接参数的保存位置：可执行文件同目录的 config.json。
func configPath() string {
	dir := "bin"
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		lower := strings.ToLower(d)
		if !strings.Contains(lower, "go-build") && !strings.Contains(lower, "\temp\\") && !strings.Contains(lower, "/tmp") {
			dir = d
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "config.json")
}

// LoadConfig 读取连接参数：环境变量优先，其次 config.json（都为空则返回零值，等界面填写）。
func LoadConfig() Config {
	var c Config
	if raw, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	if v := os.Getenv("PS_URL"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("RPC_URL"); v != "" {
		c.RPC = v
	}
	if v := os.Getenv("REST_URL"); v != "" {
		c.REST = v
	}
	if v := os.Getenv("BOX_CODE"); v != "" {
		c.Box = v
	}
	if v := os.Getenv("ORG_CODE"); v != "" {
		c.Org = v
	}
	return c
}

// Service 是暴露给前端的服务（Wails v3 会把导出的方法绑定成 JS 函数）。
type Service struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	app      *application.App
	db       *powersync.DB
	conn     *restconn.Connector
	readOnly bool
	msg      string

	watchCancel context.CancelFunc
	watchTable  string

	stop   context.CancelFunc
	stopCh chan struct{}
}

// Status 是顶部状态栏的数据。
type Status struct {
	Ready        bool   `json:"ready"`
	ReadOnly     bool   `json:"readOnly"`
	Connected    bool   `json:"connected"`
	Cursor       string `json:"cursor"`
	Buckets      int    `json:"buckets"`
	UploadQueue  int64  `json:"uploadQueue"`
	Online       bool   `json:"online"`
	LatencyMS    int64  `json:"latencyMs"`
	LastSyncedAt string `json:"lastSyncedAt"`
	Skipped      int64  `json:"skipped"`
	DBPath       string `json:"dbPath"`
	Message      string `json:"message"`
	// NeedsSetup 为 true 时说明连接参数还没填（界面应弹出设置框）
	NeedsSetup bool   `json:"needsSetup"`
	Config     Config `json:"config"`
}

// TableInfo 是左侧表清单的一项。
type TableInfo struct {
	Name string `json:"name"`
	// Rows 是未删除的行数（deleted_at 为空）
	Rows int64 `json:"rows"`
	// Deleted 是软删除行数（deleted_at 非空），界面上单独标注
	Deleted int64 `json:"deleted"`
	Alias   bool  `json:"alias"`
}

// ColumnInfo 描述一列，前端据此渲染表格与表单。
type ColumnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	IsID bool   `json:"isID"`
}

// RowsPage 是一页数据。
type RowsPage struct {
	Table   string           `json:"table"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int64            `json:"total"`
	Offset  int              `json:"offset"`
	Limit   int              `json:"limit"`
	// Deleted 是这张表的软删除行数；IncludeDeleted 表示当前是否包含它们
	Deleted        int64 `json:"deleted"`
	IncludeDeleted bool  `json:"includeDeleted"`
}

// CheckResult 是“本地 vs 后端”行数核对结果（稳定性测试用）。
type CheckResult struct {
	Table string `json:"table"`
	// Local/Remote 都是“未删除行数”
	Local  int64 `json:"local"`
	Remote int64 `json:"remote"`
	// LocalDeleted/RemoteDeleted 是软删除行数；RemoteTotal 是后端返回的总行数
	LocalDeleted  int64  `json:"localDeleted"`
	RemoteDeleted int64  `json:"remoteDeleted"`
	RemoteTotal   int64  `json:"remoteTotal"`
	OK            bool   `json:"ok"`
	Note          string `json:"note"`
}

// NewService 构造服务（不会打开数据库，Start 才打开）。
func NewService(cfg Config) *Service {
	return &Service{
		cfg:    cfg,
		log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		stopCh: make(chan struct{}),
	}
}

// AttachApp 让服务能向前端推事件。
func (s *Service) AttachApp(app *application.App) {
	s.mu.Lock()
	s.app = app
	s.mu.Unlock()
}

// Start 打开本地库并连接同步（后台执行，不阻塞 UI）。
func (s *Service) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel

	go func() {
		s.mu.Lock()
		if s.cfg.IsZero() {
			s.msg = "还没有配置连接参数（点右上角“设置”填写服务地址 / box / org）"
			s.mu.Unlock()
			s.emitStatus()
			return
		}
		s.msg = "正在打开本地库…"
		s.mu.Unlock()
		s.emitStatus()

		conn := restconn.New(restconn.Config{
			Endpoint: s.cfg.Endpoint, RPC: s.cfg.RPC, Box: s.cfg.Box, Org: s.cfg.Org,
			RESTBase: s.cfg.REST, Logger: s.log,
		})
		opts := powersync.Options{
			Filename:    s.cfg.DBPath,
			Schema:      cutterschema.Schema(),
			Connector:   conn,
			LockTimeout: 3 * time.Second,
		}
		db, err := powersync.Open(ctx, opts)
		readOnly := false
		if err != nil && strings.Contains(err.Error(), "locked") {
			// 另一个实例正在同步：降级只读（WAL 多读者），避免两个引擎写同一个库。
			opts.NoFileLock = true
			opts.Connector = nil
			if db, err = powersync.Open(ctx, opts); err == nil {
				readOnly = true
			}
		}
		if err != nil {
			s.mu.Lock()
			s.msg = "打开数据库失败: " + err.Error()
			s.mu.Unlock()
			s.log.Error("打开数据库失败", "err", err)
			s.emitStatus()
			return
		}

		s.mu.Lock()
		s.db = db
		s.conn = conn
		s.readOnly = readOnly
		if readOnly {
			s.msg = "另一个实例正在同步，本窗口为只读浏览"
		} else {
			s.msg = "正在连接同步…"
		}
		s.mu.Unlock()
		s.emitStatus()

		if !readOnly {
			if err := db.Connect(ctx); err != nil {
				s.mu.Lock()
				s.msg = "连接同步失败: " + err.Error()
				s.mu.Unlock()
				s.log.Error("Connect 失败", "err", err)
				s.emitStatus()
			} else {
				s.mu.Lock()
				s.msg = "等待首次同步…"
				s.mu.Unlock()
				s.emitStatus()
				waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
				defer waitCancel()
				if err := db.WaitForFirstSync(waitCtx); err != nil {
					s.mu.Lock()
					s.msg = "首次同步超时: " + err.Error()
					s.mu.Unlock()
				} else {
					s.mu.Lock()
					s.msg = ""
					s.mu.Unlock()
				}
			}
		}

		// 每秒推一次状态
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.emitStatus()
			}
		}
	}()
}

// Stop 断开同步并关闭库。
func (s *Service) Stop() {
	if s.stop != nil {
		s.stop()
	}
	s.mu.Lock()
	db := s.db
	s.db = nil
	s.mu.Unlock()
	if db != nil {
		db.Disconnect()
		_ = db.Close(context.Background())
	}
}

// ---------------------------------------------------------------- 前端可调用的方法

// Status 返回当前同步状态（前端也可监听 "sync:status" 事件）。
func (s *Service) Status() Status {
	return s.status()
}

// Tables 返回所有表的行数（按行数倒序）。
func (s *Service) Tables() ([]TableInfo, error) {
	db, err := s.database()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	tables := cutterschema.Schema().Tables()
	out := make([]TableInfo, 0, len(tables))
	for _, t := range tables {
		alive, deleted, err := s.countRowsBoth(ctx, db, t.Name)
		if err != nil {
			continue
		}
		out = append(out, TableInfo{
			Name: t.Name, Rows: alive, Deleted: deleted, Alias: aliasTables[t.Name],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rows != out[j].Rows {
			return out[i].Rows > out[j].Rows
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Columns 返回一张表的列定义（含类型，前端用来做表单）。
func (s *Service) Columns(table string) ([]ColumnInfo, error) {
	t, err := findTable(table)
	if err != nil {
		return nil, err
	}
	out := make([]ColumnInfo, 0, len(t.Columns)+1)
	out = append(out, ColumnInfo{Name: "id", Type: "TEXT", IsID: true})
	for _, c := range t.Columns {
		out = append(out, ColumnInfo{Name: c.Name, Type: string(c.Type)})
	}
	return out, nil
}

// Rows 返回一页数据。includeDeleted=false 时排除软删除行（deleted_at 非空）。
func (s *Service) Rows(table string, limit, offset int, includeDeleted bool) (*RowsPage, error) {
	db, err := s.database()
	if err != nil {
		return nil, err
	}
	t, err := findTable(table)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 2000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	where := ""
	if !includeDeleted && hasColumn(t, "deleted_at") {
		where = " WHERE deleted_at IS NULL"
	}
	alive, deleted, err := s.countRowsBoth(ctx, db, table)
	if err != nil {
		return nil, err
	}
	total := alive
	if includeDeleted {
		total = alive + deleted
	}

	rows, err := db.GetAll(ctx, fmt.Sprintf(`SELECT * FROM %s%s ORDER BY id LIMIT %d OFFSET %d`,
		quoteIdent(table), where, limit, offset))
	if err != nil {
		return nil, err
	}

	cols := make([]string, 0, 8)
	seen := map[string]struct{}{}
	for _, r := range rows {
		for k := range r {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			cols = append(cols, k)
		}
	}
	sort.SliceStable(cols, func(i, j int) bool {
		if cols[i] == "id" {
			return true
		}
		if cols[j] == "id" {
			return false
		}
		return cols[i] < cols[j]
	})
	return &RowsPage{
		Table: table, Columns: cols, Rows: rows, Total: total,
		Offset: offset, Limit: limit, Deleted: deleted, IncludeDeleted: includeDeleted,
	}, nil
}

// Insert 新增一行（id 未给则自动生成 UUIDv4），并触发上传。
func (s *Service) Insert(table string, values map[string]any) (string, error) {
	db, err := s.database()
	if err != nil {
		return "", err
	}
	if s.isReadOnly() {
		return "", fmt.Errorf("当前是只读模式（另一个实例在同步），无法写入")
	}
	t, err := findTable(table)
	if err != nil {
		return "", err
	}

	id, _ := values["id"].(string)
	if strings.TrimSpace(id) == "" {
		id = newID()
	}
	delete(values, "id")

	cols := []string{"id"}
	args := []any{id}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		col, ok := findColumn(t, k)
		if !ok {
			return "", fmt.Errorf("表 %s 没有列 %q", table, k)
		}
		cols = append(cols, quoteIdent(k))
		args = append(args, coerce(col.Type, values[k]))
	}

	sqlStr := fmt.Sprintf(`INSERT INTO %s(%s) VALUES(%s)`,
		quoteIdent(table), strings.Join(cols, ", "), placeholders(len(cols)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.Execute(ctx, sqlStr, args...); err != nil {
		return "", err
	}
	s.emitDataChanged(table)
	return id, nil
}

// Update 按 id 更新若干列（值为 null 表示置空）。
func (s *Service) Update(table, id string, values map[string]any) (int64, error) {
	db, err := s.database()
	if err != nil {
		return 0, err
	}
	if s.isReadOnly() {
		return 0, fmt.Errorf("当前是只读模式（另一个实例在同步），无法写入")
	}
	t, err := findTable(table)
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, fmt.Errorf("没有要更新的列")
	}

	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)

	sets := make([]string, 0, len(names))
	args := make([]any, 0, len(names)+1)
	for _, k := range names {
		col, ok := findColumn(t, k)
		if !ok {
			return 0, fmt.Errorf("表 %s 没有列 %q", table, k)
		}
		sets = append(sets, quoteIdent(k)+" = ?")
		args = append(args, coerce(col.Type, values[k]))
	}
	args = append(args, id)

	sqlStr := fmt.Sprintf(`UPDATE %s SET %s WHERE id = ?`, quoteIdent(table), strings.Join(sets, ", "))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := db.Execute(ctx, sqlStr, args...)
	if err != nil {
		return 0, err
	}
	s.emitDataChanged(table)
	return n, nil
}

// Delete 按 id 删除一行。
func (s *Service) Delete(table, id string) (int64, error) {
	db, err := s.database()
	if err != nil {
		return 0, err
	}
	if s.isReadOnly() {
		return 0, fmt.Errorf("当前是只读模式（另一个实例在同步），无法写入")
	}
	if _, err := findTable(table); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := db.Execute(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, quoteIdent(table)), id)
	if err != nil {
		return 0, err
	}
	s.emitDataChanged(table)
	return n, nil
}

// Watch 监听一张表的本地变化（同步下来的 + 本地写的），有变化就推 "data:changed"。
func (s *Service) Watch(table string) error {
	db, err := s.database()
	if err != nil {
		return err
	}
	if _, err := findTable(table); err != nil {
		return err
	}
	s.mu.Lock()
	if s.watchCancel != nil {
		s.watchCancel()
		s.watchCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.watchCancel = cancel
	s.watchTable = table
	s.mu.Unlock()

	ch, err := db.WatchTables(ctx, []string{table},
		fmt.Sprintf(`SELECT count(*) AS n FROM %s`, quoteIdent(table)))
	if err != nil {
		cancel()
		return err
	}
	go func() {
		for range ch {
			s.emitDataChanged(table)
		}
	}()
	return nil
}

// Unwatch 停止监听。
func (s *Service) Unwatch() {
	s.mu.Lock()
	if s.watchCancel != nil {
		s.watchCancel()
		s.watchCancel = nil
	}
	s.watchTable = ""
	s.mu.Unlock()
}

// Check 核对“本地 vs 后端”的行数（同口径比较，软删除行单独统计）。
//
// 注意：本地列表默认不显示软删除行（deleted_at 非空），而后端 REST 可能仍返回它们，
// 所以这里把两边都按“未删除行数”对齐比较，并把软删除数量一起报出来，避免误会。
func (s *Service) Check(table string) (*CheckResult, error) {
	db, err := s.database()
	if err != nil {
		return nil, err
	}
	t, err := findTable(table)
	if err != nil {
		return nil, err
	}
	if s.readOnly {
		return &CheckResult{Table: table, Note: "只读模式，未连接后端"}, nil
	}
	if err := s.conn.EnsureToken(context.Background()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	local, localDeleted, err := s.countRowsBoth(ctx, db, table)
	if err != nil {
		return nil, err
	}
	res := &CheckResult{Table: table, Local: local, LocalDeleted: localDeleted, Remote: -1}

	selectCols := "id"
	if hasColumn(t, "deleted_at") {
		selectCols = "id,deleted_at"
	}
	var raw []map[string]json.RawMessage
	if err := s.conn.REST().GetJSON(ctx, "/"+table+"?select="+selectCols+"&limit=200000", &raw); err != nil {
		res.Note = "后端无此表/REST 不可达: " + err.Error()
		return res, nil
	}
	var remoteAlive, remoteDeleted int64
	for _, r := range raw {
		if d, ok := r["deleted_at"]; ok && isNonNullJSON(d) {
			remoteDeleted++
			continue
		}
		remoteAlive++
	}
	res.Remote = remoteAlive
	res.RemoteTotal = int64(len(raw))
	res.RemoteDeleted = remoteDeleted

	res.OK = local == remoteAlive
	switch {
	case local == remoteAlive && localDeleted == remoteDeleted:
		res.Note = fmt.Sprintf("本地与后端一致（未删除 %d，软删除 %d）", local, localDeleted)
	case local == remoteAlive:
		res.Note = fmt.Sprintf("未删除行一致（%d）；软删除本地 %d / 后端 %d（不影响）", local, localDeleted, remoteDeleted)
	case local < remoteAlive:
		res.Note = fmt.Sprintf("后端多 %d 行（sync rules 过滤 / 其它端刚写入，正常）", remoteAlive-local)
	default:
		res.Note = fmt.Sprintf("本地多 %d 行：可能还没上传完", local-remoteAlive)
	}
	return res, nil
}

// isNonNullJSON 判断 JSON 值不是 null。
func isNonNullJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// SyncNow 重新连接同步（用于断线后手动重连）。
func (s *Service) SyncNow() error {
	s.mu.Lock()
	db := s.db
	readOnly := s.readOnly
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("数据库还没打开")
	}
	if readOnly {
		return fmt.Errorf("只读模式（另一个实例在同步）")
	}
	db.Disconnect()
	if err := db.Connect(context.Background()); err != nil {
		return err
	}
	return nil
}

// NeedsSetup 报告连接参数是否还没填。
func (s *Service) NeedsSetup() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.IsZero()
}

// SaveConfig 保存连接参数到 config.json 并重新连接（界面“设置”用）。
func (s *Service) SaveConfig(cfg Config) error {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.RPC = strings.TrimSpace(cfg.RPC)
	cfg.Box = strings.TrimSpace(cfg.Box)
	cfg.Org = strings.TrimSpace(cfg.Org)
	cfg.REST = strings.TrimSpace(cfg.REST)
	if cfg.IsZero() {
		return fmt.Errorf("服务地址 / RPC / box_code / org_code 都不能为空")
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := configPath()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	s.log.Info("已保存连接参数", "path", path, "endpoint", cfg.Endpoint, "box", cfg.Box, "org", cfg.Org)

	// 重新启动：先停掉旧的同步与库，再用新参数打开
	s.mu.Lock()
	oldDB := s.db
	dbPath := s.cfg.DBPath
	s.cfg = cfg
	s.cfg.DBPath = dbPath
	s.db = nil
	s.conn = nil
	s.readOnly = false
	s.mu.Unlock()
	if oldDB != nil {
		oldDB.Disconnect()
		_ = oldDB.Close(context.Background())
	}
	if s.stop != nil {
		s.stop()
	}
	s.Start()
	return nil
}

// ---------------------------------------------------------------- 内部

func (s *Service) database() (*powersync.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.msg != "" {
			return nil, fmt.Errorf("%s", s.msg)
		}
		return nil, fmt.Errorf("数据库还没打开")
	}
	return s.db, nil
}

func (s *Service) isReadOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readOnly
}

func (s *Service) status() Status {
	s.mu.Lock()
	db, readOnly, msg := s.db, s.readOnly, s.msg
	s.mu.Unlock()

	st := Status{
		Ready: db != nil, ReadOnly: readOnly, DBPath: s.cfg.DBPath, Message: msg,
		NeedsSetup: s.cfg.IsZero(), Config: s.cfg,
	}
	if db == nil {
		return st
	}
	cur := db.CurrentStatus()
	net := db.CurrentNetworkStatus()
	st.Connected = cur.Connected
	st.Cursor = cur.Cursor
	st.Buckets = cur.Buckets
	if !cur.LastSyncedAt.IsZero() {
		// 库里存的是 UTC，界面按东八区显示（不依赖系统 tzdata）
		st.LastSyncedAt = cur.LastSyncedAt.In(beijingZone).Format("01-02 15:04:05")
	}
	st.Online = net.Online
	st.LatencyMS = net.Latency.Milliseconds()
	st.Skipped = db.SkippedUnknownOps()
	if n, err := db.UploadQueue(context.Background()); err == nil {
		st.UploadQueue = n
	}
	return st
}

func (s *Service) emitStatus() {
	s.mu.Lock()
	app := s.app
	s.mu.Unlock()
	if app == nil {
		return
	}
	app.Event.Emit("sync:status", s.status())
}

func (s *Service) emitDataChanged(table string) {
	s.mu.Lock()
	app := s.app
	s.mu.Unlock()
	if app == nil {
		return
	}
	app.Event.Emit("data:changed", table)
}

// countRowsBoth 同时返回“未删除行数”和“软删除行数”（没有 deleted_at 列时后者为 0）。
func (s *Service) countRowsBoth(ctx context.Context, db *powersync.DB, table string) (alive, deleted int64, err error) {
	if _, ok := findTableLoose(table); !ok {
		return 0, 0, fmt.Errorf("schema 里没有表 %q", table)
	}
	rows, err := db.GetAll(ctx, fmt.Sprintf(
		`SELECT count(*) AS total, sum(CASE WHEN deleted_at IS NULL THEN 1 ELSE 0 END) AS alive FROM %s`,
		quoteIdent(table)))
	if err == nil && len(rows) == 1 {
		total := toInt64(rows[0]["total"])
		a := toInt64(rows[0]["alive"])
		return a, total - a, nil
	}
	// 没有 deleted_at 列：全部算未删除
	rows, err = db.GetAll(ctx, fmt.Sprintf(`SELECT count(*) AS n FROM %s`, quoteIdent(table)))
	if err != nil {
		return 0, 0, err
	}
	if len(rows) != 1 {
		return 0, 0, fmt.Errorf("count 查询没有结果")
	}
	return toInt64(rows[0]["n"]), 0, nil
}

// findTableLoose 判断表是否在 schema 里。
func findTableLoose(name string) (powersync.Table, bool) {
	for _, t := range cutterschema.Schema().Tables() {
		if t.Name == name {
			return t, true
		}
	}
	return powersync.Table{}, false
}

// hasColumn 判断表是否有某一列。
func hasColumn(t powersync.Table, name string) bool {
	_, ok := findColumn(t, name)
	return ok
}

func (s *Service) countRows(ctx context.Context, db *powersync.DB, table string) (int64, error) {
	if rows, err := db.GetAll(ctx, fmt.Sprintf(`SELECT count(*) AS n FROM %s WHERE deleted_at IS NULL`, quoteIdent(table))); err == nil && len(rows) == 1 {
		return toInt64(rows[0]["n"]), nil
	}
	rows, err := db.GetAll(ctx, fmt.Sprintf(`SELECT count(*) AS n FROM %s`, quoteIdent(table)))
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("count 查询没有结果")
	}
	return toInt64(rows[0]["n"]), nil
}

func findTable(name string) (powersync.Table, error) {
	for _, t := range cutterschema.Schema().Tables() {
		if t.Name == name {
			return t, nil
		}
	}
	return powersync.Table{}, fmt.Errorf("schema 里没有表 %q", name)
}

func findColumn(t powersync.Table, name string) (powersync.Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return powersync.Column{}, false
}

// coerce 按列类型把前端传来的值转成合适的 Go 类型（JSON 数字默认 float64）。
func coerce(colType powersync.ColumnType, v any) any {
	switch colType {
	case powersync.IntegerColumn:
		switch x := v.(type) {
		case float64:
			return int64(x)
		case int64:
			return x
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return n
			}
			return x
		}
	case powersync.RealColumn:
		switch x := v.(type) {
		case float64:
			return x
		case int64:
			return float64(x)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return f
			}
			return x
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	return v
}

func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ", ")
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// newID 生成 UUIDv4（真实后端主键多为 uuid 列）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1e12)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func defaultDBPath() string {
	// 默认放在 **可执行文件同目录**（即 examples/wailsapp/bin/app.db），
	// 方便直接看到/备份/删掉重建本地库；用 -db 参数可以换。
	dir := "bin"
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		// `go run .` 时 exe 在临时目录（go-build…），改用当前目录下的 bin
		lower := strings.ToLower(d)
		if !strings.Contains(lower, "go-build") && !strings.Contains(lower, "\\temp\\") && !strings.Contains(lower, "/tmp") {
			dir = d
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "app.db")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Log 让前端把错误/调试信息打到 Go 的日志里（WebView 的 console 看不到时很有用）。
func (s *Service) Log(msg string) {
	s.log.Info("frontend", "msg", msg)
	fmt.Println("[frontend]", msg)
}
