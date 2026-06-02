# 91

<p align="center">
  <img width="120" height="120" alt="91" src="https://github.com/user-attachments/assets/5b323c94-bbd3-4dce-bbc8-adc86935b7de" />
</p>

<p align="center">
  😄 个人私有视频站 😄
</p>

<p align="center">
  <a href="#快速开始">快速开始</a> ·
  <a href="#功能特性">功能特性</a> ·
  <a href="#预览图">预览图</a> ·
  <a href="#数据存放位置">数据目录</a> ·
  <a href="#许可证">许可证</a>
</p>

---

## 功能特性

- **多后端支持** — 兼容 115 云盘、PikPak 云盘、OneDrive、Google Drive 和本地存储
- **低带宽播放** — 115 云盘、PikPak 云盘、OneDrive 都支持302模式，在线播放视频时，不占用服务器带宽，播放体验不受服务器带宽影响；Google Drive 不支持302模式，走服务器中转，观看体验会受服务器带宽影响
- **封面 & 预览片段** — 自动为每个视频生成封面图和预览片段，首页快速选片
- **91 爬虫** — 内置爬虫，支持抓取 91 本月最热视频
- **双主题** — 黑黄经典主题 / 粉白清新主题，随时切换
- **短视频模式** — 一键切换抖音风格，沉浸刷片
- **低资源占用** — 2C2G 服务器稳定运行，主要性能消耗就是封面图和预览视频的生成

---

## 预览图

### 电脑端

<p>
  <img width="49%" alt="首页" src="https://github.com/user-attachments/assets/9808fceb-760b-4dd5-b7d2-8622b95b90d5" />
  <img width="49%" alt="播放页" src="https://github.com/user-attachments/assets/859db4aa-1fba-44f2-bb46-1db07c2f964f" />
</p>

<p>
  <img width="49%" alt="主题切换" src="https://github.com/user-attachments/assets/96bea37a-8764-413e-9b70-1856b4ae0cd2" />
  <img width="49%" alt="管理页" src="https://github.com/user-attachments/assets/29c1e27a-7651-4dfc-93dd-556331844214" />
</p>

### 手机端

<p align="center">
  <img width="1284" height="1134" alt="手机端" src="https://github.com/user-attachments/assets/bdb7a86c-a4e5-483e-a307-e02c0bb34dac" />
</p>

---

## 快速开始

### 方式一：一键安装脚本（推荐）

```bash
sudo apt update && sudo apt install -y curl ca-certificates
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/install.sh -o install.sh
sudo bash install.sh
```

部署完成后访问：

| 地址 | 说明 |
|------|------|
| `http://服务器IP:9191/` | 前台 |
| `http://服务器IP:9191/admin` | 后台管理 |

**注意：如果首次访问，显示502，可以运行 `91 restart` 重启一下服务**

安装后自动注册 `91` 管理命令：

```bash
91            # 打开管理菜单
91 status     # 查看运行状态
91 logs       # 查看日志
91 update     # 更新到最新版本
91 restart    # 重启服务
91 stop       # 停止服务
```

> `video-site-91` 为等效别名，两者可互换使用。

**自定义端口：**

```bash
FRONTEND_PORT=8080 sudo -E bash install.sh
```

**旧版本升级（v0.0.2 之前）：**

旧版脚本直接执行 `91 update` 可能失败，先执行以下修复命令：

```bash
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/install.sh -o /tmp/install-91.sh
sudo bash /tmp/install-91.sh update
```

---

### 方式二：Docker Compose 部署

**1. 准备目录**

```bash
mkdir -p video-site-91 && cd video-site-91
```

**2. 创建 `docker-compose.yml`**

```yaml
services:
  video-site-91:
    image: ghcr.io/nianzhibai/91:stable
    container_name: video-site-91
    ports:
      - "9191:9191"
    volumes:
      - ./data:/opt/video-site-91/data
    restart: unless-stopped
```
创建yml文件后运行下面指令
```bash
docker compose pull
docker compose up -d
```

如果想固定某个 Release 版本，可以改成明确的 tag，例如：

```yaml
image: ghcr.io/nianzhibai/91:v0.0.6
```

或直接拉取仓库内置配置：

```bash
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/docker-compose.yml -o docker-compose.yml
```

**3. 启动**

```bash
docker compose up -d
```

**常用命令：**

```bash
docker compose logs -f       # 查看日志
docker compose pull          # 拉取最新正式版 stable 镜像
docker compose up -d         # 更新并重启
```

> 所有配置、数据库、封面、预览及上传文件均保存在 `./data/` 目录下。

---

## 数据存放位置

### 一键脚本部署

| 路径 | 内容 |
|------|------|
| `/opt/video-site-91/config.yaml` | 配置文件、管理员账号、网盘凭证 |
| `/opt/video-site-91/data/video-site.db` | SQLite 数据库 |
| `/opt/video-site-91/data/previews/` | 封面图和预览片段 |

### Docker Compose 部署

| 路径 | 内容 |
|------|------|
| `./data/config.yaml` | 配置文件、管理员账号、网盘凭证 |
| `./data/video-site.db` | SQLite 数据库 |
| `./data/previews/` | 封面图和预览片段 |
| `./data/uploads/` | 本地上传的视频文件 |
| `./data/spider91/` | 91 爬虫抓取的视频文件 |

---

## 更多文档

| 文档 | 内容 |
|------|------|
| [backend/README.md](backend/README.md) | 后端实现、接口说明、网盘字段 |
| [video-site-implementation-plan.md](video-site-implementation-plan.md) | 完整实现方案 |

---

## 使用须知

本项目面向**个人私有部署**，请仅接入你有权访问和管理的内容，并遵守对应网盘、站点的服务条款及所在地法律法规。

> 不对外传播，仅限个人使用。

---

## 许可证

本项目基于 [MIT License](LICENSE) 开源。

---

## 致谢

- [OpenList](https://github.com/OpenListTeam/OpenList) — 优秀的开源项目
- [LinuxDo](https://linux.do/) — 学 AI 上 L 站
- [NodeSeek](https://nodeseek.com/) — MJJ 上 N 站
