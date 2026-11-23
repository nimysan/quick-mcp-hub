package applemail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，免 cgo
)

// ErrFullDiskAccess 表示被 macOS TCC 拦下：~/Library/Mail 需要「完全磁盘访问权限」。
// 用 errors.Is 判断；错误文本里已经带了去哪里授权的说明，可以原样给用户看。
var ErrFullDiskAccess = errors.New("无权读取 Apple Mail 数据（macOS 隐私保护拦截）。" +
	"请打开「系统设置 → 隐私与安全性 → 完全磁盘访问权限」，" +
	"把运行 mcp-hub 的程序（mcp-hub 本身，或启动它的终端 / Kiro / Claude 等宿主应用）加入并打开开关，" +
	"然后完全退出并重新启动该程序")

// ErrFilePermission 表示普通的文件权限不足（例如目录被 chmod 成不可读、属于别的用户）。
// 这和 macOS 隐私保护无关，开完全磁盘访问权限没有用。
var ErrFilePermission = errors.New("当前用户没有读取 Apple Mail 数据的文件权限")

// ErrNotFound 表示找不到 Apple Mail 数据目录、邮件或其 .emlx 文件。
var ErrNotFound = errors.New("未找到")

// coreDataEpoch 是 2001-01-01 相对 Unix 纪元的秒数；个别版本的列用这个纪元。
const coreDataEpoch = 978307200

// Store 是一个打开的 Apple Mail 本地库。并发安全（底层是 *sql.DB）。
type Store struct {
	root    string // ~/Library/Mail
	base    string // ~/Library/Mail/V<n>
	version string // "V10"
	db      *sql.DB
	cols    map[string]map[string]bool // 表 → 列集合
	epoch   int64                      // 数据库时间值加上它得到 Unix 秒
	dbPath  string
	// immutable 为真表示以 immutable=1 打开（看不到 WAL 里未合并的最新改动）：
	// 要么 -wal / -shm 不全（避免新建文件），要么 mode=ro 打不开（fallback）。
	immutable bool
	fallback  bool
}

// sidecarsExist 报告 -wal 和 -shm 是否都已存在。
func sidecarsExist(dbPath string) bool {
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suf); err != nil {
			return false
		}
	}
	return true
}

// Stale 报告这个连接是否该重开：因缺 -wal / -shm 用了 immutable=1，
// 而现在两个都出现了（Mail.app 启动了），重开后能用 mode=ro 读到最新数据。
func (s *Store) Stale() bool {
	return s.immutable && !s.fallback && sidecarsExist(s.dbPath)
}

// DefaultRoot 返回 ~/Library/Mail。
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("~", "Library", "Mail")
	}
	return filepath.Join(home, "Library", "Mail")
}

var versionDirRe = regexp.MustCompile(`^V(\d+)$`)

// DetectVersion 在 root 下找编号最大、且含 MailData/Envelope Index 的 V<n> 目录。
// 按数字比较（V10 > V9），不按字符串。
func DetectVersion(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", wrapFSErr(root, err)
	}
	type cand struct {
		n    int
		name string
	}
	var cands []cand
	for _, e := range entries {
		m := versionDirRe.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		cands = append(cands, cand{n, e.Name()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].n > cands[j].n })
	for _, c := range cands {
		_, err := os.Stat(filepath.Join(root, c.name, "MailData", "Envelope Index"))
		if err == nil {
			return c.name, nil
		}
		if errors.Is(err, fs.ErrPermission) {
			return "", wrapFSErr(filepath.Join(root, c.name), err)
		}
	}
	return "", fmt.Errorf("%w: %s 下没有含 MailData/Envelope Index 的 V<n> 目录（这台 Mac 可能没用过「邮件」App）", ErrNotFound, root)
}

// Open 打开 root（空串表示 ~/Library/Mail）下最新版本的 Envelope Index，只读。
func Open(root string) (*Store, error) {
	if root == "" {
		root = DefaultRoot()
	}
	ver, err := DetectVersion(root)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, ver)
	dbPath := filepath.Join(base, "MailData", "Envelope Index")

	// 先用普通文件读探一次：TCC 拒绝时 SQLite 只会报含糊的 "unable to open database file"。
	f, err := os.Open(dbPath)
	if err != nil {
		return nil, wrapFSErr(dbPath, err)
	}
	f.Close()

	s := &Store{root: root, base: base, version: ver, dbPath: dbPath}
	// 硬约束：不在 Mail 的数据目录里新建任何文件。SQLite 用 mode=ro 读 WAL 库时，
	// 缺 -wal 或 -shm 会自己建出来（Mail.app 正常退出后两个通常都不在）。
	// 所以两个都已存在才用 mode=ro（共享锁 + 读 WAL，能看到 Mail.app 刚写入的数据）；
	// 缺任一个就用 immutable=1：只读主库，不碰也不建 -wal / -shm。
	// immutable 时 Mail 一旦启动就会出现这两个文件，Stale 据此报告过期，由上层重新打开。
	// shortcut: 检查和打开之间 Mail 恰好退出并删掉两个文件时，mode=ro 仍会重建它们；
	// 窗口只有微秒级，要彻底消除得换成能拒绝建文件的 VFS。
	imm := !sidecarsExist(dbPath)
	db, err := openRO(dbPath, imm)
	if err != nil && !imm {
		// mode=ro 打不开（例如 -shm 不可写）才退回 immutable=1。immutable 跳过 WAL，
		// Mail.app 正在写时可能读到旧数据甚至 "malformed"，所以只作后备。
		db2, err2 := openRO(dbPath, true)
		if err2 != nil {
			return nil, fmt.Errorf("只读打开 %s 失败: %v（immutable 后备也失败: %v）", dbPath, err, err2)
		}
		db, err, s.fallback = db2, nil, true
	}
	if err != nil {
		return nil, fmt.Errorf("只读打开 %s 失败: %w", dbPath, err)
	}
	s.immutable = imm || s.fallback
	s.db = db
	if err := s.loadSchema(); err != nil {
		db.Close()
		return nil, err
	}
	if !s.cols["messages"]["rowid"] || !s.cols["mailboxes"]["url"] {
		db.Close()
		return nil, fmt.Errorf("%s 的表结构不认识（缺 messages 或 mailboxes.url），可能是未适配的 Mail 版本 %s", dbPath, ver)
	}
	s.epoch = s.detectEpoch()
	return s, nil
}

func openRO(path string, immutable bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(3000)"
	if immutable {
		q = "mode=ro&immutable=1&_pragma=query_only(1)"
	}
	db, err := sql.Open("sqlite", u.String()+"?"+q)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

// Version 返回探测到的数据目录版本，例如 "V10"。
func (s *Store) Version() string { return s.version }

// Immutable 报告是否以 immutable 模式打开。
func (s *Store) Immutable() bool { return s.immutable }

func (s *Store) loadSchema() error {
	s.cols = map[string]map[string]bool{}
	for _, t := range []string{"messages", "subjects", "addresses", "mailboxes", "recipients", "attachments", "summaries"} {
		rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", t)
		if err != nil {
			return fmt.Errorf("读取表结构 %s 失败: %w", t, err)
		}
		set := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				set[strings.ToLower(name)] = true
			}
		}
		rows.Close()
		if len(set) > 0 {
			// INTEGER PRIMARY KEY 的表都有隐式 ROWID，pragma 不一定列出。
			set["rowid"] = true
			s.cols[t] = set
		}
	}
	return nil
}

func (s *Store) has(table, col string) bool { return s.cols[table][col] }

// detectEpoch 用最大的时间值判断纪元：Unix 秒还是 CoreData(2001) 秒。
func (s *Store) detectEpoch() int64 {
	col := "date_received"
	if !s.has("messages", col) {
		return 0
	}
	var v sql.NullInt64
	_ = s.db.QueryRow("SELECT MAX(" + col + ") FROM messages").Scan(&v)
	return inferEpoch(v.Int64, time.Now().Unix())
}

func inferEpoch(sample, now int64) int64 {
	if sample <= 0 {
		return 0
	}
	plausible := func(u int64) bool { y := time.Unix(u, 0).UTC().Year(); return y >= 1990 && y <= 2100 }
	unixOK, coreOK := plausible(sample), plausible(sample+coreDataEpoch)
	switch {
	case coreOK && !unixOK:
		return coreDataEpoch
	case unixOK && !coreOK:
		return 0
	}
	if abs(sample+coreDataEpoch-now) < abs(sample-now) {
		return coreDataEpoch
	}
	return 0
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (s *Store) toTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v+s.epoch, 0)
}

func (s *Store) fromTime(t time.Time) int64 { return t.Unix() - s.epoch }

// wrapFSErr 把 macOS 隐私保护（TCC）的拦截统一成 ErrFullDiskAccess，普通文件权限不足
// 统一成 ErrFilePermission，不存在统一成 ErrNotFound。
func wrapFSErr(path string, err error) error {
	switch {
	case IsTCCDenied(err):
		return fmt.Errorf("%w（被拒绝的路径: %s，原始错误: %v）", ErrFullDiskAccess, path, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w（路径: %s）: %w", ErrFilePermission, path, err)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return err
}

// IsTCCDenied 认出 macOS 隐私保护的拦截：TCC 拒绝时系统调用返回 EPERM
// （"operation not permitted"）；普通的 Unix 权限不足返回的是 EACCES（"permission denied"），
// 那种情况开完全磁盘访问权限没有用。
func IsTCCDenied(err error) bool {
	return errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "operation not permitted")
}

// denied 报告 err 是不是读不了（隐私保护或文件权限），这两种都要报错，不能当成「没有正文」。
func denied(err error) bool {
	return errors.Is(err, ErrFullDiskAccess) || errors.Is(err, ErrFilePermission)
}
