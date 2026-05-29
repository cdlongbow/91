# 91

<p align="center">
  <img width="120" height="120" alt="91" src="https://github.com/user-attachments/assets/5b323c94-bbd3-4dce-bbc8-adc86935b7de" />
</p>

<p align="center">
  😄个人 91 站😄
</p>

---

## 项目说明

支持 115 云盘、PikPak 云盘作为视频播放后端。

采用 115 云盘和 PikPak 云盘的 302 重定向播放，不占用服务器带宽，也不会因为服务器带宽小而影响视频播放体验。

服务器只负责扫描云盘中的视频文件，并给每个视频生成封面图和预览片段。

你可以通过封面图和预览片段，在首页快速选择想看的视频。

支持 91 爬虫，爬取 91 的本月最热视频。

内置两种主题：黑黄主题（91 经典主题）和粉白主题。

支持短视频模式，一键切换成熟悉的抖音模式。

该项目2C2G服务器稳定跑👍👍👍

---

## 预览图

### 电脑端

<p>
  <img width="49%" alt="91 电脑端首页" src="https://github.com/user-attachments/assets/9808fceb-760b-4dd5-b7d2-8622b95b90d5" />
  <img width="49%" alt="91 电脑端播放页" src="https://github.com/user-attachments/assets/859db4aa-1fba-44f2-bb46-1db07c2f964f" />
</p>

<p>
  <img width="49%" alt="91 电脑端主题" src="https://github.com/user-attachments/assets/96bea37a-8764-413e-9b70-1856b4ae0cd2" />
  <img width="49%" alt="91 电脑端管理页" src="https://github.com/user-attachments/assets/29c1e27a-7651-4dfc-93dd-556331844214" />
</p>

### 手机端

<p align="center">
  <img width="1284" height="1134" alt="PixPin_2026-05-29_11-54-12" src="https://github.com/user-attachments/assets/bdb7a86c-a4e5-483e-a307-e02c0bb34dac" />
</p>

---

## 快速开始

一键安装：

```bash
sudo apt update
sudo apt install -y curl ca-certificates
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/install.sh -o install.sh
sudo bash install.sh
```

部署完成后访问：

- 前台：`http://服务器IP:9191/`
- 后台：`http://服务器IP:9191/admin`

如果发现无法访问，可以运行 `91 restart` 重启一下项目

安装后会自动创建 `91` 指令：

```bash
91          # 打开管理菜单
91 status   # 查看状态
91 logs     # 查看日志
91 update   # 更新
91 restart  # 重启
91 stop     # 停止
```

同时也保留 `video-site-91` 作为同等别名。

旧版本用户升级说明：

如果你是在 `v0.0.2` 之前部署的项目，系统里可能还保留旧的 `91` 管理脚本。旧脚本直接运行 `91 update` 可能更新失败。先执行下面的一次性修复命令，后续再使用 `91 update` 即可：

```bash
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/install.sh -o /tmp/install-91.sh
sudo bash /tmp/install-91.sh update
```

想换端口：

```bash
FRONTEND_PORT=8080 sudo -E bash install.sh
```

---

## 数据存放位置

项目会把运行数据保存在本地：

- `/opt/video-site-91/config.yaml`：本地配置、管理员账号、网盘凭证。
- `/opt/video-site-91/data/video-site.db`：SQLite 数据库。
- `/opt/video-site-91/data/previews/`：本地生成的封面和 teaser。

---

## 了解更多

根目录 README 只保留项目介绍和最短上手路径。更细的实现、接口、网盘字段和部署方式可以看：

- [backend/README.md](backend/README.md)
- [video-site-implementation-plan.md](video-site-implementation-plan.md)

---

## 使用边界

这个项目面向个人私有部署。请只接入你有权访问和管理的内容，并遵守对应网盘、站点服务条款以及所在地法律法规。

不要传播，仅限个人使用，个人视频站。

---

## 致谢

感谢开源项目 OpenList。

感谢 <a href="https://linux.do/">LinuxDo</a> 社区，学 AI 上 L 站。

感谢 <a href="https://nodeseek.com/">NodeSeek</a> 社区，MJJ 上 N 站。
