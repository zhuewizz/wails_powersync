# wails_power — PowerSync 双向同步桌面演示（Wails v3.1）

从 `powersync_golang/examples/wailsapp` 复制出来的**独立项目**：用 Wails v3 + Go + 原生 JS 做的桌面应用，
演示 local-first 双向同步（本地 SQLite 读写 → 自动上传 → 服务端回显）。

## 功能

| 区域 | 内容 |
| --- | --- |
| 顶栏 | 连接状态、网络延迟、cursor、buckets、待上传队列、最近同步时间（东八区）；重连 / 刷新 / 新增 |
| 左栏 | 全部 65 张表（行数倒序，`+N` 表示软删除行数；可过滤、只看有数据），点一下看数据 |
| 右侧 | 数据表格（分页）、每行「编辑 / 删除」、`核对后端`（本地 vs 后端行数，同口径） |
| 弹窗 | 新增/编辑表单：按 schema 渲染每一列（带类型），可显式写 NULL |
| 自动刷新 | 本地写与同步下发都会通过事件推送刷新界面；状态每秒推送 |

## 运行

```powershell
cd E:\wails_power

wails3 dev            # 开发模式（热重载）
wails3 build          # 打包 → bin\wailsapp.exe
.\bin\wailsapp.exe    # 直接运行

# 不开窗口自检（同步 + 表清单，可选 -writes 验证增改删、-check 核对某表）
.\bin\wailsapp.exe -selftest
.\bin\wailsapp.exe -selftest -writes
.\bin\wailsapp.exe -selftest -check users
```

## 本地数据库

- 默认在 **可执行文件同目录**：`bin\app.db`（`-db` 可改）
- 想重置：关掉程序 → 删掉 `bin\app.db*` → 重开，会全量重同步
- `*.db`、`bin/`、`frontend/dist`、`frontend/node_modules`、`frontend/bindings` 都在 `.gitignore` 里

## 连接参数（不写死）

代码里**没有任何硬编码**的地址/柜号/组织，按优先级取：

| 优先级 | 来源 | 说明 |
| --- | --- | --- |
| 1 | 环境变量 | `PS_URL` / `RPC_URL` / `REST_URL` / `BOX_CODE` / `ORG_CODE` |
| 2 | `binconfig.json` | 界面「⚙ 设置」保存后写入（已 gitignore） |
| 3 | 界面填写 | 首次启动未配置时自动弹出设置框 |

```json
{
  "endpoint": "http://<your-powersync-host>:8080",
  "rpc": "http://<your-postgrest-host>:8089/rest/rpc/fetch_credentials",
  "rest": "http://<your-postgrest-host>:8089/rest",
  "box": "<box_code>",
  "org": "<org_code>"
}
```

```powershell
$env:PS_URL="http://<your-powersync-host>:8080"
$env:REST_URL="http://<your-postgrest-host>:8089/rest"
$env:RPC_URL="http://<your-postgrest-host>:8089/rest/rpc/fetch_credentials"
$env:BOX_CODE="<box_code>"; $env:ORG_CODE="<org_code>"
```

## 依赖关系（重要）

`go.mod` 里通过 replace 指向**本地 SDK 仓库**：

```
require github.com/zhuewizz/powersync_golang v0.0.0
replace github.com/zhuewizz/powersync_golang => ../powersync_golang
```

所以目录结构需要是：

```
E:\
├── powersync_golang\     ← SDK（本项目的依赖）
└── wails_power\          ← 本项目
```

如果 SDK 将来发布到 GitHub，把 replace 换成远端模块即可：

```bash
go mod edit -dropreplace=msmkSync
go mod edit -require=github.com/zhuewizz/powersync_golang@v0.1.0
```

（当前 SDK 的 `module` 名是 `msmkSync`，发布前需改成 `github.com/<org>/msmkSync`。）

## 结构

```
wails_power/
├── main.go              # Wails app：窗口、事件注册、-selftest/-writes/-check
├── service.go           # 暴露给前端：Status/Tables/Columns/Rows/Insert/Update/Delete/Watch/Check/SyncNow/Log
├── frontend/
│   ├── index.html
│   └── src/{main.js, style.css}
├── build/               # Wails 打包配置
└── bin/                 # 构建产物 + 本地数据库（git 忽略）
```

## 说明

- 仓库只保留 **Windows** 的打包模板（`build/windows` + `build/Taskfile.yml`）；
  需要 macOS/Linux/iOS/Android 打包时，从 Wails v3 模板把对应 `build/<platform>` 目录补回来即可。

- 客户端 ↔ 服务：HTTP `/sync/stream` 长响应流（NDJSON/BSON，非 WebSocket）；上传是普通 HTTP（PostgREST）。
  详见 SDK 仓库的 `FAQ.md`。
- 同一个 `app.db` 只能有一个实例同步；再开一个会自动降级为只读浏览。

## GitHub

已经推上去了：

| 仓库 | 说明 |
| --- | --- |
| https://github.com/zhuewizz/wails_powersync | **本项目**（Wails 桌面版） |
| https://github.com/zhuewizz/powersync_golang | SDK（**public**，tag v0.1.0） |

日常推送：

```powershell
cd E:\wails_power
git add -A
git commit -m "feat: ..."
git push
```

> 两个仓库都是 **public**，可直接 clone / `go get`。

### 别处 clone 后怎么编译

本项目 go.mod 用 replace 指向本地 SDK 目录（方便同时改 SDK 和界面），所以：

```powershell
# 两个仓库放成兄弟目录
git clone https://github.com/zhuewizz/powersync_golang E:\powersync_golang
git clone https://github.com/zhuewizz/wails_powersync  E:\wails_power

cd E:\wails_power
wails3 build        # frontend 依赖 + bindings + 前端产物都会自动生成
```

如果想直接用远端 SDK 版本（不 clone SDK）：

```powershell
go mod edit -dropreplace=github.com/zhuewizz/powersync_golang
go mod edit -require=github.com/zhuewizz/powersync_golang@v0.1.0
go mod tidy
```
