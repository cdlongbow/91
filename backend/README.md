# backend

视频聚合站的 Go 后端。提供三件事：

1. 多家网盘统一抽象（夸克 / 115 / PikPak / 联通沃盘 / OneDrive / 本地存储）
2. 视频元数据目录（SQLite）+ 扫描 + teaser 预生成
3. REST API（前台）+ 管理后台 + 直链代理
4. 标签池、视频隐藏、按网盘统计和详情页来源网盘类型展示能力

## 目录

```
cmd/server/main.go          入口
internal/
  config/                   YAML 配置
  catalog/                  SQLite 元数据
  drives/
    iface.go                Drive 接口
    quark/                  夸克（自己实现，参考 OpenList quark_uc）
    p115/                   115（壳子 + SheltonZhu/115driver）
    pikpak/                 PikPak（自己实现，参考 OpenList pikpak）
    wopan/                  联通沃盘（壳子 + OpenListTeam/wopan-sdk-go）
    onedrive/               OneDrive（OpenList 在线续期 + Microsoft Graph 文件接口）
    localstorage/           本地目录扫描（服务器已有视频目录）
  scanner/                  扫目录 → 落库
  preview/                  ffmpeg 抽封面和生成多段 teaser
  proxy/                    /p/stream/*、/p/preview/* 代理
  auth/                     管理员 session
  api/                      REST 路由
config.example.yaml         配置模板
```

## 开发环境（Windows）

本仓库假设工具都装在用户目录，不需要管理员权限。

```
C:\Users\<you>\tools\
  go\bin\go.exe             Go 1.23+
  ffmpeg\bin\ffmpeg.exe     任意 ≥ 4.x 版本
```

并加到 `PATH`。

### 第一次启动

Git Bash / WSL 环境推荐从仓库根目录启动完整开发环境：

```bash
npm install
./start.sh               # 默认前端 production preview，无热更新
```

需要前端开发热更新时再用 `FRONTEND_MODE=dev ./start.sh --restart`。

PowerShell 下可以分两个终端手动启动，后端命令如下：

```powershell
cd F:\VideoProject\backend
go run ./cmd/server
```

首次启动会在当前目录创建：

- `config.yaml`（从 `config.example.yaml` 复制）
- `data/video-site.db`
- `data/previews/`

默认监听 `127.0.0.1:9192`。首次部署如果仍是默认管理员配置，登录页会要求先设置用户名和密码，并写回 `config.yaml`。如果本地已有旧的 `config.yaml`，请确认 `server.listen` 与前端代理端口一致。

### 连接前端

`vite.config.ts` 已经把 `/api`、`/p`、`/admin/api` 代理到 `127.0.0.1:9192`。

```
npm run build       构建前端静态资源
npm run preview     前端 9191，无热更新
go run ./cmd/server 后端 9192
```

## 添加一个盘

推荐在前端管理后台 `/admin/drives` 新增网盘。保存后会立即挂载并触发扫描；视频结果可在 `/admin/videos` 按网盘查看，每页 100 条，页面会同时显示各网盘 Teaser 已生成、待生成、失败数量。

也可以直接调用后端接口：

1. 先在浏览器访问 `/login` 完成首次管理员设置，或使用已有管理员账号登录：`POST /admin/api/login`
2. 新建盘：`POST /admin/api/drives`
   ```json
   {
     "id":   "my-quark",
     "kind": "quark",
     "name": "我的夸克盘",
     "rootId": "0",
     "scanRootId": "0",
     "credentials": {
       "cookie": "粘贴浏览器 F12 复制的 pan.quark.cn Cookie"
     }
   }
   ```
3. 手动触发扫描：`POST /admin/api/drives/my-quark/rescan`

各网盘的凭证字段：

| kind   | credentials 字段                                              |
|--------|---------------------------------------------------------------|
| quark  | `cookie`                                                      |
| p115   | `cookie`（形如 `UID=...; CID=...; SEID=...; KID=...`）         |
| pikpak | `username`、`password`（token、验证码和设备 ID 由服务端自动处理并保存） |
| wopan  | `access_token`、`refresh_token`，可选 `family_id`              |
| onedrive | `refresh_token` |
| localstorage | `path`（服务器上的已有视频目录，如 `/mnt/videos`） |

### PikPak 速度说明

`disable_media_link` 默认按 `true` 处理，会使用 PikPak 的 `web_content_link` 原始下载链接；在当前服务器实测，单连接通常只有约 2.8-3 MiB/s。把该字段设置为 `false` 后，驱动会请求 `usage=CACHE` 并优先使用 `medias[].link.url`，当前服务器实测 `/p/stream` 64 MiB Range 可到约 8.9 MiB/s。

当前服务器同时存在 sing-box TUN 透明代理，PikPak 默认出站会被 `tun0` 接管；但强制直连物理网卡并没有更快，慢速的主要差异来自 PikPak 取链方式。media/cache CDN 节点仍有波动，偶尔可能遇到慢节点；如果播放变慢，可重新获取直链或重新挂载 PikPak 后再测。

OneDrive 按 OpenList 默认应用方式调用 `https://api.oplist.org/onedrive/renewapi` 在线刷新 token，不需要配置 Azure 应用的 `client_id` / `client_secret` / `redirect_uri`。后台新建 OneDrive 时只需要填 OpenList 代刷得到的 `refresh_token`；服务端会默认挂载根目录并自动回写新 token。

## 文件名约定

扫描器按以下顺序解析文件名：

1. `[tag1,tag2] 标题 - 作者.mp4`
2. `[tag1,tag2] 标题.mp4`
3. `标题 - 作者.mp4`
4. `标题.mp4`

标签分隔符支持 `, ， 、` 和空格。解析结果会和系统标签池匹配，常见番号类噪声会归并到 `AV` 等系统标签，避免把每个番号都变成独立标签。解析结果可在管理后台覆盖。

## 视频去重

项目有三层去重：

1. 同一网盘同一文件按 `(drive_id, file_id)` 形成稳定视频 ID，重复扫描只更新同一行。
2. 扫描时优先按网盘侧 `content_hash` 去重；没有 hash 时退化为 `file_name + size_bytes`。
3. 扫描、爬虫、本地上传或服务启动挂载网盘后，后台指纹 worker 会异步读取视频的少量 Range 片段，生成 `sampled_sha256`。前台列表、首页、搜索、推荐会按 `size_bytes + sampled_sha256` 只展示最早入库的 canonical 视频。

`sampled_sha256` 是文件级去重：适合识别同一个视频文件被复制到 115 / PikPak / OneDrive 等不同网盘的情况。它不会删除任何网盘文件，也不用于识别转码、裁剪、加水印后的同源视频。

封面和 teaser 仍然优先生成，不等待指纹完成。夜间流水线最后会做一次重复资产清理：对 `size_bytes + sampled_sha256` 命中的非 canonical 视频，只删除本机生成的重复封面和 teaser，并把对应字段重置为 `pending`。网盘原文件和视频元数据记录不会被删除；如果 canonical 视频以后被移除，这些重复项会重新进入生成队列。

## 管理能力

- `/admin/drives`：新增、编辑、删除网盘，触发扫描。
- `/admin/videos`：按网盘筛选视频，每页 100 条分页，查看各网盘 Teaser 统计，编辑标题/作者/分类/标签，单条或全量重生 teaser。
- `/admin/tags`：新增标签并用内置规则自动匹配已有视频。
- 播放页视频信息会展示来源网盘类型；同时提供“不再展示”，点击后会把视频标记为全局隐藏。隐藏视频不会再出现在首页、列表、搜索、相关推荐和详情接口中。目前没有管理后台恢复入口，如需恢复可把数据库里对应视频的 `hidden` 字段改回 `0`。

## Teaser 生成

scanner 扫到新视频会把 `(driveID, videoID)` 丢进 worker 队列。worker 会先用 `ffprobe` 探测时长，再用 `ffmpeg` 抽封面和生成无声 teaser：

```
ffmpeg -ss <起点> -headers "UA/Cookie/Referer" -i <直链> \
       -t 3 -an -vf scale=480:-2 -c:v libx264 -preset veryfast -crf 28 \
       -movflags +faststart -y <local>.mp4
```

当前策略是每段固定 3 秒；30 秒以下最多 3 段，30 秒及以上固定 4 段；长视频在 20% 到 80% 区间均匀取段。生成的 teaser 和封面都只保存在本地 `data/previews/`，不会回写到网盘；旧数据中的 `preview_file_id` 会被忽略。

服务启动或网盘重新挂载时，如果 Teaser 开关已开启，后端会把历史 `pending` 任务重新入队，避免重启后长期停在“待生成”。OneDrive 扫盘和直链生成 teaser / 封面时可能触发 Microsoft Graph 429、`TooManyRequests`、`activityLimitReached` 或 throttled 文本；后端会识别这类错误并让当前网盘进入冷却期，保留任务为 `pending`，避免连续请求触发更严重限流。扫盘阶段会按 `Retry-After` 或默认冷却时间等待后继续当前目录。

前端卡片的 `previewSrc` 统一指向 `/p/preview/<videoID>`，后端只从本地 `preview_local` 文件读取。

## 验证

```bash
# 前端，在仓库根目录执行
npm run lint
npm run build
node --test tests/previewIntent.test.ts

# 后端，在 backend/ 执行
go test ./... -count=1
```

## 部署到 Linux

推荐先使用根目录的预编译安装脚本：

```bash
sudo bash install.sh
```

它会从 GitHub Release 下载预编译包，安装运行依赖、写入 systemd 服务并启动。下面是手动部署方式，适合你想自己接管构建和服务管理时使用。

```bash
# 交叉编译
GOOS=linux GOARCH=amd64 go build -o video-server ./cmd/server

# 目标机
sudo apt install ffmpeg
scp video-server user@host:/opt/video-site/
ssh user@host
cd /opt/video-site
cp config.example.yaml config.yaml
# 改密码、监听地址
./video-server
```

配 systemd + nginx 反代到 `/` 和 `/api`、`/p`、`/admin`。
