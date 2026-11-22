// Command wailsapp 是 Wails v3 桌面版演示：把 msmkSync 的双向同步能力做成可点的界面。
//
// 页面能力：
//   - 左侧：所有表的清单（行数、同步状态），点一下就看这张表的数据
//   - 右侧：数据表格（分页/刷新）、新增、编辑、删除
//   - 顶部：同步状态（连接、cursor、buckets、待上传队列、网络、最近同步时间）
//
// 数据源与 examples/demo 完全一致：cutterschema（来自 syncRules.yaml）+ 真实服务端。
//
// 运行（在 examples/wailsapp 目录）：
//
//	wails3 dev          # 开发模式（热重载）
//	wails3 build        # 打包 exe
//	go run .            # 直接跑（需先构建过一次前端，frontend/dist 存在）
package main

// 语言统计触发标记（可删）

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

// init 注册事件类型，让 wails3 generate bindings 生成带类型的 JS API。
func init() {
	application.RegisterEvent[Status]("sync:status")
	application.RegisterEvent[string]("data:changed")
}

func main() {
	dbFlag := flag.String("db", defaultDBPath(), "本地数据库文件")
	selftest := flag.Bool("selftest", false, "不开窗口：打开库+同步+打印表清单后退出（CI/自检用）")
	writes := flag.Bool("writes", false, "配合 -selftest：再跑一遍 增→改→删（验证 Service 的写路径）")
	checkT := flag.String("check", "", "配合 -selftest：核对某张表的 本地/后端 行数（如 users）")
	flag.Parse()

	// 连接参数不写死：环境变量 > 可执行文件同目录的 config.json > 界面里填写
	cfg := LoadConfig()
	cfg.DBPath = *dbFlag
	svc := NewService(cfg)

	app := application.New(application.Options{
		Name:        "PowerSync 双向同步演示",
		Description: "PowerSync + Wails v3：表清单 / 增删改 / 同步状态",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// 把 app 交给 service：状态变化、数据变化用事件推给前端。
	svc.AttachApp(app)

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "PowerSync 双向同步演示",
		Width:  1280,
		Height: 820,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(18, 18, 22),
		URL:              "/",
	})

	// 后台打开库 + 连接同步（不阻塞窗口显示）。
	svc.Start()

	// 自检模式：不创建窗口，等首次同步后打印每张表行数，然后退出。
	if *selftest {
		runSelftest(svc)
		if *writes {
			runWriteTest(svc)
		}
		if *checkT != "" {
			runCheck(svc, *checkT)
		}
		svc.Stop()
		return
	}

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
	svc.Stop()
}

// runSelftest 是 -selftest 的实现：验证 Go 侧（库 + 同步 + 表清单）通不通，不开窗口。
func runSelftest(svc *Service) {
	if svc.NeedsSetup() {
		fmt.Println("未配置连接参数：请在界面里点“设置”填写，或设置环境变量 PS_URL / RPC_URL / BOX_CODE / ORG_CODE")
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		st := svc.Status()
		// 等到首次同步完成（cursor 有值）——只连上还不够
		if st.Ready && (st.Connected || st.ReadOnly) && st.Cursor != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	st := svc.Status()
	fmt.Printf("状态: ready=%v connected=%v readOnly=%v cursor=%s buckets=%d uploadQueue=%d lastSynced=%s", st.Ready, st.Connected, st.ReadOnly, st.Cursor, st.Buckets, st.UploadQueue, st.LastSyncedAt)
	fmt.Println()
	if !st.Ready {
		fmt.Fprintf(os.Stderr, "自检失败: %s", st.Message)
		fmt.Println()
		os.Exit(1)
	}

	tables, err := svc.Tables()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取表清单失败: %v", err)
		fmt.Println()
		os.Exit(1)
	}
	var total int64
	nonEmpty := 0
	for _, t := range tables {
		total += t.Rows
		if t.Rows > 0 {
			nonEmpty++
		}
	}
	fmt.Printf("表清单: %d 张（有数据 %d 张），合计 %d 行", len(tables), nonEmpty, total)
	fmt.Println()

	showed := 0
	for _, t := range tables {
		if t.Rows == 0 || showed >= 10 {
			continue
		}
		mark := ""
		if t.Alias {
			mark = "  *"
		}
		fmt.Printf("  %-28s %8d%s", t.Name, t.Rows, mark)
		fmt.Println()
		showed++
	}

	fmt.Println("自检完成（加 -writes 可再验证 增/改/删）")
}

// runWriteTest 验证 Service 的增/改/删（界面按钮走的就是这几个方法），用完即删干净。
func runWriteTest(svc *Service) {
	const table = "sys_error_report"
	fmt.Println("写测试：", table)

	cols, err := svc.Columns(table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Columns 失败:", err)
		os.Exit(1)
	}
	fmt.Printf("  列数: %d（id 自动生成）\n", len(cols))

	values := map[string]any{
		"org_code":   svc.cfg.Org,
		"box_code":   svc.cfg.Box,
		"box_cell":   "A1",
		"box_type":   "1",
		"user_id":    "u1",
		"user_name":  "wails-selftest",
		"error_date": "2026-01-01 00:00:00",
		"error_desc": "wails 写测试",
		"trans_type": "PROBE",
		"op_status":  "PROBE",
	}
	id, err := svc.Insert(table, values)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Insert 失败:", err)
		os.Exit(1)
	}
	fmt.Println("  Insert ok, id =", id)

	page, err := svc.Rows(table, 500, 0, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Rows 失败:", err)
		os.Exit(1)
	}
	found := false
	for _, r := range page.Rows {
		if r["id"] == id {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "Insert 后查不到这一行")
		os.Exit(1)
	}
	fmt.Println("  Rows 能查到新行 ✓")

	if _, err := svc.Update(table, id, map[string]any{"op_status": "UPDATED", "error_desc": "wails 写测试-已改"}); err != nil {
		fmt.Fprintln(os.Stderr, "Update 失败:", err)
		os.Exit(1)
	}
	fmt.Println("  Update ok ✓")

	if _, err := svc.Delete(table, id); err != nil {
		fmt.Fprintln(os.Stderr, "Delete 失败:", err)
		os.Exit(1)
	}
	fmt.Println("  Delete ok ✓（本地删除会进上传队列，稍后自动同步到后端）")
}

// runCheck 打印一张表的本地/后端行数核对结果（含软删除统计）。
func runCheck(svc *Service, table string) {
	res, err := svc.Check(table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Check 失败:", err)
		return
	}
	fmt.Printf("核对 %s: 未删除 本地=%d 后端=%d；软删除 本地=%d 后端=%d；后端总行数=%d；结论=%v",
		res.Table, res.Local, res.Remote, res.LocalDeleted, res.RemoteDeleted, res.RemoteTotal, res.OK)
	fmt.Println()
	fmt.Println("  说明:", res.Note)
}
