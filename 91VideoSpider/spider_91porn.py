#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
91porn 视频爬虫脚本
===================
爬取 https://www.91porn.com/v.php?category=top&viewtype=basic 下的所有视频信息：
  - 视频名称
  - 封面图直链
  - 视频直链 (MP4)

依赖安装:
    pip install requests beautifulsoup4 lxml

使用方法:
    # 全量爬取（默认行为，从 page=1 一直爬到末尾，写到 OUTPUT_FILE）
    python spider_91porn.py

    # 只爬指定页（单页模式，手动调试用）
    python spider_91porn.py --page 1 --output /tmp/spider91_page1.json

    # 凑够 N 个新视频模式（backend 凌晨任务用）
    python spider_91porn.py --target-new 15 --seen-viewkeys-file /tmp/seen.txt --output /tmp/new.json

CLI 参数:
    --page N                  只爬第 N 页，配合 --output 用于手动调试
    --target-new N            从 page 1 起翻页直到凑够 N 个新视频（不在 seen 列表里的）
    --seen-viewkeys-file FILE 每行一个已知 viewkey 或 mp4 源 ID，命中即跳过；与 --target-new 配合使用
    --output FILE             输出 JSON 路径，覆盖默认的 OUTPUT_FILE
    --no-resume               禁用断点续爬（单页/target-new 模式下自动禁用）
    --quiet                   压缩日志，每条视频只输出一行
    -h / --help               帮助

配置说明 (编辑脚本内 "配置区域"):
    - MIN_PAGE_DELAY / MAX_PAGE_DELAY : 列表页请求间隔 (默认 3-6 秒)
    - MIN_DETAIL_DELAY / MAX_DETAIL_DELAY : 详情页请求间隔 (默认 2-5 秒)
    - MAX_PAGES : 限制最大爬取页数 (None=不限, 如 5=只爬前5页)
    - OUTPUT_FILE : 输出文件名

输出格式 (JSON):
    {
      "videos": [
        {
          "title": "视频标题",
          "thumb_url": "https://...thumb/xxxx.jpg",
          "video_url": "https://...mp43/xxxx.mp4?st=...",
          "viewkey": "abc123...",
          "source_id": "xxxx",
          "detail_url": "https://...view_video.php?viewkey=..."
        },
        ...
      ]
    }

注意:
    1. 视频直链包含时效性token (e参数为过期时间戳)，会过期，需定期重新爬取
    2. 脚本已内置随机延时，请勿移除，避免对服务器造成压力
    3. 网站有Cloudflare保护，如遇到403/5xx错误，可能需要使用带cookie的session
    4. 本脚本仅供学习交流，请遵守当地法律法规

作者: OpenCode
日期: 2026-05-22
"""

import argparse
import requests
import re
import time
import random
import json
import os
import socket
import sys
import html
from urllib.parse import urljoin, unquote, urlparse
from datetime import datetime

try:
    from bs4 import BeautifulSoup
except ImportError:
    print("错误: 缺少依赖库 beautifulsoup4")
    print("请运行: pip install beautifulsoup4 lxml")
    sys.exit(1)


def prefer_ipv4_for_plain_socks5_proxy():
    """PySocks may pick IPv6 first for socks5://; some SOCKS5 servers only accept IPv4."""
    proxy_envs = (
        os.environ.get("HTTPS_PROXY", ""),
        os.environ.get("HTTP_PROXY", ""),
        os.environ.get("https_proxy", ""),
        os.environ.get("http_proxy", ""),
    )
    uses_plain_socks5 = any(v.strip().lower().startswith("socks5://") for v in proxy_envs)
    if not uses_plain_socks5 or getattr(socket, "_spider91_ipv4_first", False):
        return

    original_getaddrinfo = socket.getaddrinfo

    def getaddrinfo_ipv4_first(*args, **kwargs):
        infos = original_getaddrinfo(*args, **kwargs)
        return sorted(infos, key=lambda info: 0 if info[0] == socket.AF_INET else 1)

    socket.getaddrinfo = getaddrinfo_ipv4_first
    socket._spider91_ipv4_first = True

# ===================== 配置区域 =====================
BASE_URL = "https://www.91porn.com/v.php"
LIST_PARAMS = {
    "category": "top",
    "viewtype": "basic"
}

# 请求头 (模拟真实浏览器)
HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/125.0.0.0 Safari/537.36"
    ),
    "Accept": (
        "text/html,application/xhtml+xml,application/xml;"
        "q=0.9,image/avif,image/webp,image/apng,*/*;"
        "q=0.8,application/signed-exchange;v=b3;q=0.7"
    ),
    "Accept-Language": "zh-CN,zh;q=0.9",
    # 注意: 不要包含 "br" (brotli)，除非安装了 brotli 库
    # "Accept-Encoding": "gzip, deflate, br",
    "Connection": "keep-alive",
    "Upgrade-Insecure-Requests": "1",
    "Sec-Fetch-Dest": "document",
    "Sec-Fetch-Mode": "navigate",
    "Sec-Fetch-Site": "none",
    "Sec-Fetch-User": "?1",
}

# 延时配置 (秒) - 控制爬取频率，避免被封
MIN_PAGE_DELAY = 3.0      # 列表页之间最小延时
MAX_PAGE_DELAY = 6.0      # 列表页之间最大延时
MIN_DETAIL_DELAY = 2.0    # 详情页之间最小延时
MAX_DETAIL_DELAY = 5.0    # 详情页之间最大延时

# 重试配置
MAX_RETRIES = 3
RETRY_DELAY = 5.0

# 输出配置
OUTPUT_FILE = "91porn_videos.json"
MAX_PAGES = None          # 设置为 None 爬取所有页，或设置整数如 5 只爬前5页
RESUME = True             # 是否跳过输出文件中已存在的 viewkey (断点续爬)
MAX_EMPTY_PAGES = 2       # 连续空页数达到此值时停止爬取
# ===================================================


class Porn91Spider:
    def __init__(
        self,
        output_file: str = None,
        start_page: int = 1,
        max_pages: int = None,
        resume: bool = None,
        max_empty_pages: int = None,
        quiet: bool = False,
        target_new: int = None,
        seen_viewkeys: list = None,
        stream_output: bool = False,
    ):
        """
        构造函数。所有参数都有默认值，等同于使用脚本顶部的全局配置。
        backend 调用时会传 output_file/seen_viewkeys/target_new，等价于：
            "从第 1 页开始爬，跳过 seen_viewkeys 里的视频，凑够 target_new 个新视频后停止"

        stream_output=True 时（backend 流水线用）：
            - 每凑齐一个 video 直链就把该 entry 作为一行 JSON 写到 stdout 并 flush，
              便于上层（Go crawler）边读边下载，不再等所有详情页处理完。
            - 所有日志改走 stderr，避免与 stdout JSONL 流混合。
            - --output 仍生效，作为离线归档用（脚本退出时一次性写完整 JSON）。
        """
        self.session = requests.Session()
        self.session.headers.update(HEADERS)
        # 91porn 没有固定 mode cookie 时，详情页首次请求可能返回与列表卡片
        # 不一致的视频源；固定桌面模式让列表页和详情页解析保持一致。
        self.session.cookies.set("mode", "d")

        # 解析后的实际配置；优先使用构造参数，回退到模块级配置
        self.output_file = output_file if output_file is not None else OUTPUT_FILE
        self.start_page = max(1, int(start_page or 1))
        # max_pages=None 表示不限制；max_pages=N 表示从 start_page 起爬 N 页
        self.max_pages = max_pages if max_pages is None or max_pages > 0 else None
        # resume 默认跟模块配置；单页模式下调用方应该显式传 False
        self.resume = RESUME if resume is None else bool(resume)
        self.max_empty_pages = (
            MAX_EMPTY_PAGES if max_empty_pages is None else int(max_empty_pages)
        )
        # target_new 是 backend 触发时的核心模式：累计处理这么多新源视频后退出。
        self.target_new = target_new if target_new and target_new > 0 else None
        self.quiet = bool(quiet)
        # stream_output：每解析出一个 video 直链立即输出一行 JSON 到 stdout
        # （配合 backend Go 端 bufio.Scanner 实时消费，下载一个就开始下一个）。
        # 开启后所有 log 都走 stderr。
        self.stream_output = bool(stream_output)

        # 添加重试适配器
        try:
            from requests.adapters import HTTPAdapter
            from urllib3.util.retry import Retry
            retry_strategy = Retry(
                total=MAX_RETRIES,
                backoff_factor=1,
                status_forcelist=[429, 500, 502, 503, 504],
            )
            adapter = HTTPAdapter(max_retries=retry_strategy)
            self.session.mount("https://", adapter)
            self.session.mount("http://", adapter)
        except ImportError:
            pass  # urllib3 版本可能较低

        self.results = []
        self.pages_crawled = 0
        self.processed_videos = 0
        self.skipped_videos = 0
        self.failed_videos = 0
        self.skip_viewkeys = set()

        # backend 通过 --seen-viewkeys-file 传进来一批已入库的历史 ID。
        # 兼容旧名：文件里可能是 viewkey，也可能是新逻辑使用的 mp4 源 ID。
        if seen_viewkeys:
            for vk in seen_viewkeys:
                if not vk:
                    continue
                vk = vk.strip()
                if vk:
                    self.skip_viewkeys.add(vk)

        # 断点续爬：加载已有结果，跳过已处理的 viewkey
        if self.resume and os.path.exists(self.output_file):
            try:
                with open(self.output_file, 'r', encoding='utf-8') as f:
                    existing_data = json.load(f)
                existing_videos = existing_data.get('videos', [])
                self.results = existing_videos
                for v in existing_videos:
                    vk = v.get('viewkey', '')
                    if vk:
                        self.skip_viewkeys.add(vk)
                self.processed_videos = existing_data.get('successful', 0)
                self.failed_videos = existing_data.get('failed', 0)
                self.log(f"加载已有数据: {len(self.results)} 个视频, 将跳过已处理项")
            except Exception:
                pass

    def log(self, message: str):
        """带时间戳的日志输出。stream_output 模式下走 stderr，避免污染 stdout JSONL。"""
        timestamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        line = f"[{timestamp}] {message}"
        if self.stream_output:
            print(line, file=sys.stderr, flush=True)
        else:
            print(line)

    def emit_stream_video(self, video: dict):
        """stream_output 模式下把单条 video entry 作为一行 JSON 写到 stdout 并立即刷盘。
        Go 端 bufio.Scanner 按行读取，每收到一行就立即下载视频和封面。"""
        if not self.stream_output:
            return
        try:
            print(json.dumps(video, ensure_ascii=False), flush=True)
        except Exception as e:
            # stdout 异常基本只在管道断开时发生（消费方进程死了）；
            # 写到 stderr 让 backend 看到，然后让 crawl 循环自己 break。
            print(f"[stream] emit failed: {e}", file=sys.stderr, flush=True)

    def random_sleep(self, min_sec: float, max_sec: float):
        """随机延时，模拟人类行为"""
        delay = random.uniform(min_sec, max_sec)
        if not self.quiet:
            self.log(f"  随机延时 {delay:.2f} 秒...")
        time.sleep(delay)

    def fetch_page(self, url: str, description: str = "", referer: str = "") -> str:
        """
        获取页面HTML内容，带错误处理和重试
        """
        headers_extra = {}
        if referer:
            headers_extra["Referer"] = referer

        for attempt in range(1, MAX_RETRIES + 1):
            try:
                self.log(f"正在请求: {description or url} (尝试 {attempt}/{MAX_RETRIES})")
                response = self.session.get(url, timeout=30, headers=headers_extra)

                # 检查是否被Cloudflare拦截 (需在 raise_for_status 之前)
                if response.status_code == 403:
                    self.log("警告: 收到 403 Forbidden，可能被拦截")
                    if attempt < MAX_RETRIES:
                        self.random_sleep(RETRY_DELAY, RETRY_DELAY + 3)
                        continue
                    return ""

                response.raise_for_status()

                # 优先使用 content.decode('utf-8')，避免 requests 编码检测问题
                try:
                    html_content = response.content.decode('utf-8', errors='replace')
                except Exception:
                    html_content = response.text

                # Cloudflare 挑战检测：如果页面主要内容只有挑战页面，而非正常内容
                # 注意：网站本身会加载 challenge-platform 脚本，所以不能仅凭此判断
                is_cf_challenge = (
                    "Just a moment" in html_content and
                    len(html_content) < 8000
                )
                if is_cf_challenge:
                    self.log("警告: 页面被Cloudflare挑战拦截，需要浏览器环境或正确cookie")
                    if attempt < MAX_RETRIES:
                        self.random_sleep(RETRY_DELAY, RETRY_DELAY + 5)
                        continue
                    return ""

                return html_content
            except requests.exceptions.HTTPError as e:
                self.log(f"HTTP错误: {e}")
                if attempt < MAX_RETRIES:
                    self.random_sleep(RETRY_DELAY, RETRY_DELAY + 3)
                else:
                    return ""
            except requests.exceptions.RequestException as e:
                self.log(f"请求失败: {e}")
                if attempt < MAX_RETRIES:
                    self.random_sleep(RETRY_DELAY, RETRY_DELAY + 3)
                else:
                    self.log(f"达到最大重试次数，放弃: {url}")
                    return ""
        return ""

    def parse_list_page(self, html: str) -> list:
        """
        解析列表页，提取视频基本信息
        返回: [{title, detail_url, thumb_url, viewkey}, ...]
        """
        videos = []
        soup = BeautifulSoup(html, 'lxml')

        # 只解析正常视频卡片。页面中还混有 col-lg-8 的异常大卡片，里面的标题、
        # thumb、detail URL 会串到其它视频，不能作为入库来源。
        video_cards = soup.select('div.col-xs-12.col-sm-4.col-md-3.col-lg-3')

        seen_cards = set()

        for card in video_cards:
            link = card.find('a', href=re.compile(r'view_video\.php\?viewkey='))
            if not link:
                continue
            href = link.get('href', '')
            if not href:
                continue

            # 提取 viewkey
            match = re.search(r'viewkey=([^&]+)', href)
            if not match:
                continue
            viewkey = match.group(1)

            detail_url = urljoin(BASE_URL, href)

            # 提取标题
            title = self._extract_title(link)

            # 提取列表卡片来源 ID 和封面图 URL
            thumb_url = ""
            source_id = ""
            overlay = link.find(id=re.compile(r'^playvthumb_\d+$'))
            if overlay:
                source_id = overlay.get('id', '').rsplit('_', 1)[-1]
            img = link.find('img', class_=re.compile(r'img-responsive'))
            if img:
                thumb_url = img.get('src', '') or img.get('data-original', '')
                if thumb_url:
                    thumb_url = urljoin(BASE_URL, thumb_url)
            if not source_id and thumb_url:
                source_id = self._extract_thumb_source_id(thumb_url)

            card_key = source_id or detail_url
            if card_key in seen_cards:
                continue
            seen_cards.add(card_key)

            videos.append({
                "title": title,
                "detail_url": detail_url,
                "thumb_url": thumb_url,
                "viewkey": viewkey,
                "source_id": source_id
            })

        return videos

    def _extract_title(self, link) -> str:
        """
        从视频链接标签中提取并清理标题
        """
        # 优先从 span.video-title 获取 (已渲染的干净标题)
        title_el = link.find('span', class_=re.compile(r'video-title'))
        if title_el:
            title = title_el.get_text(strip=True)
            if title:
                return html.unescape(title)

        # 备用: 从 link 的 title 属性提取
        title = link.get('title', '').strip()
        if title:
            return html.unescape(title)

        # 最后手段: 从链接文本提取并清理前缀
        text = link.get_text(separator=' ', strip=True)
        # 去掉前缀: "HD" / "91" / 时间戳 "HH:MM:SS"
        text = re.sub(r'^(HD\s+|91\s+)?\d{2}:\d{2}:\d{2}\s*', '', text)
        text = re.sub(r'\s+', ' ', text).strip()
        return html.unescape(text)[:120]

    def parse_detail_page(self, html: str) -> dict:
        """
        解析详情页，提取视频直链
        返回: {"video_url": "...", "source_id": "...", "title": "..."} 或空字典
        """
        result = {}

        if not html:
            return result

        title = self._extract_detail_title(html)
        if title:
            result["title"] = title

        # 方法1: 解码 strencode2 (主要方式, 页面通过 document.write 动态写入 video 标签)
        # 格式: document.write(strencode2("%3c%73%6f..."));
        strencode_match = re.search(r'strencode2\(["\']([^"\']+)["\']\)', html)
        if strencode_match:
            encoded = strencode_match.group(1)
            try:
                # strencode2 在JS中等价于 unescape / decodeURIComponent
                decoded = unquote(encoded)

                # 从解码后的 HTML 片段中提取 src
                src_match = re.search(r"src=['\"]([^'\"]+)['\"]", decoded)
                if src_match:
                    video_url = src_match.group(1)
                    # 规范化双斜杠 (如 https://host//path -> https://host/path)
                    video_url = re.sub(r'(https?://[^/]+)//+', r'\1/', video_url)
                    result["video_url"] = video_url
                    result["source_id"] = self._extract_source_id(video_url)
                    return result
            except Exception as e:
                self.log(f"  解码 strencode2 失败: {e}")

        # 方法2: 通用正则匹配页面中的 mp4 链接 (备用, 过滤广告)
        mp4_match = re.search(
            r'https?://[^\s"\'<>]+\.mp4[^\s"\'<>]*',
            html
        )
        if mp4_match:
            url = mp4_match.group(0)
            if 'kwai' not in url and 'ad-' not in url.lower():
                result["video_url"] = url
                result["source_id"] = self._extract_source_id(url)
                return result

        return result

    def _extract_detail_title(self, html_text: str) -> str:
        soup = BeautifulSoup(html_text, 'lxml')
        title_el = soup.find('title')
        if not title_el:
            return ""
        title = title_el.get_text(" ", strip=True)
        title = re.sub(r'\s*-\s*91porn.*$', '', title, flags=re.IGNORECASE).strip()
        return html.unescape(title)[:160]

    def _extract_source_id(self, video_url: str) -> str:
        path = urlparse(video_url or "").path
        name = os.path.basename(path)
        stem, ext = os.path.splitext(name)
        if ext.lower() not in {".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi"}:
            return ""
        source_id = re.sub(r'[^0-9]+', '', stem)
        if not source_id or source_id != stem:
            return ""
        return source_id

    def _extract_thumb_source_id(self, thumb_url: str) -> str:
        path = urlparse(thumb_url or "").path
        match = re.search(r'/thumb/(\d+)\.[A-Za-z0-9]+$', path)
        return match.group(1) if match else ""

    def _thumb_url_for_source(self, thumb_url: str, source_id: str) -> str:
        if not thumb_url or not source_id:
            return thumb_url
        parsed = urlparse(thumb_url)
        match = re.search(r'/thumb/([^/?#]+)\.[A-Za-z0-9]+$', parsed.path)
        if not match:
            return thumb_url
        current = match.group(1)
        if current == source_id:
            return thumb_url
        path = re.sub(
            r'/thumb/[^/?#]+\.[A-Za-z0-9]+$',
            f'/thumb/{source_id}.jpg',
            parsed.path,
        )
        return parsed._replace(path=path, query="", fragment="").geturl()

    def crawl(self):
        """
        主爬取流程。停止条件（任一满足即停）：
          - 达到 max_pages 配置
          - 连续 max_empty_pages 页都没有视频
          - target_new 模式下，已经累计处理 target_new 个新视频
        """
        self.log("=" * 60)
        self.log("91porn 视频爬虫启动")
        self.log("=" * 60)
        self.log(f"配置: 列表页延时 {MIN_PAGE_DELAY}-{MAX_PAGE_DELAY}s, 详情页延时 {MIN_DETAIL_DELAY}-{MAX_DETAIL_DELAY}s")
        self.log(f"配置: 最大重试 {MAX_RETRIES} 次, 连续空页上限 {self.max_empty_pages}")
        self.log(f"配置: 起始页 {self.start_page}, 最大爬取页数 {self.max_pages if self.max_pages else '不限'}")
        if self.target_new:
            self.log(f"配置: 目标新增视频数 {self.target_new}")
        self.log(f"配置: 输出文件 {os.path.abspath(self.output_file)}")
        if self.skip_viewkeys:
            self.log(f"配置: 已跳过 {len(self.skip_viewkeys)} 个已知 viewkey")
        self.log("")

        page_num = self.start_page
        consecutive_empty = 0
        crawled_in_session = 0

        while True:
            if self.max_pages is not None and crawled_in_session >= self.max_pages:
                self.log(f"达到配置的页数上限 {self.max_pages}，停止")
                break
            if consecutive_empty >= self.max_empty_pages:
                self.log(f"连续 {self.max_empty_pages} 页无结果，已达到末尾")
                break
            if self.target_new is not None and self.processed_videos >= self.target_new:
                self.log(f"已累计 {self.processed_videos} 个新视频，达到目标 {self.target_new}，停止")
                break

            if page_num == 1:
                page_url = f"{BASE_URL}?category=top&viewtype=basic"
            else:
                page_url = f"{BASE_URL}?category=top&viewtype=basic&page={page_num}"

            if crawled_in_session > 0:
                self.log("")
                self.random_sleep(MIN_PAGE_DELAY, MAX_PAGE_DELAY)

            self.log(f"[页 {page_num}] 请求: {page_url}")
            page_html = self.fetch_page(page_url, f"列表页 第{page_num}页")

            if not page_html:
                self.log(f"[页 {page_num}] 获取失败，跳过")
                consecutive_empty += 1
                page_num += 1
                crawled_in_session += 1
                continue

            page_videos = self.parse_list_page(page_html)

            # 判断页面是否真的没有视频（而非全部已处理）
            if not page_videos:
                self.log(f"[页 {page_num}] 页面无视频，可能已到末尾")
                consecutive_empty += 1
                page_num += 1
                crawled_in_session += 1
                continue

            consecutive_empty = 0

            # 过滤已处理的 viewkey，只保留新视频
            new_videos = [v for v in page_videos if v['viewkey'] not in self.skip_viewkeys]
            skipped_on_page = len(page_videos) - len(new_videos)

            if skipped_on_page > 0:
                self.log(f"[页 {page_num}] 发现 {len(page_videos)} 个链接, 其中 {skipped_on_page} 个已处理, {len(new_videos)} 个新视频")
            else:
                self.log(f"[页 {page_num}] 发现 {len(new_videos)} 个视频")

            if new_videos:
                self._process_video_list(new_videos, referer=page_url)
            self.pages_crawled += 1
            page_num += 1
            crawled_in_session += 1

        self._save_results()
        self._print_summary()

    def _process_video_list(self, videos: list, referer: str = ""):
        """
        处理一批视频列表，逐个获取详情页
        """
        for idx, video in enumerate(videos, 1):
            # target_new 模式下，凑够后立即停止，不再请求详情页
            if self.target_new is not None and self.processed_videos >= self.target_new:
                return
            # 跳过已处理的 viewkey (断点续爬)
            if video['viewkey'] in self.skip_viewkeys:
                self.log(f"  [SKIP] 已处理过: {video['viewkey']}")
                self.skipped_videos += 1
                continue

            self.log(f"  处理视频 {idx}/{len(videos)}: {video['title'][:40]}...")

            # 延时控制 (同一批次内第一个视频不延时)
            if idx > 1:
                self.random_sleep(MIN_DETAIL_DELAY, MAX_DETAIL_DELAY)

            # 获取详情页
            detail_html = self.fetch_page(video['detail_url'], f"详情页 viewkey={video['viewkey']}", referer=referer)

            if not detail_html:
                self.log(f"  [FAIL] 详情页获取失败: {video['viewkey']}")
                video["video_url"] = ""
                self.results.append(video)
                self.skip_viewkeys.add(video['viewkey'])
                self.failed_videos += 1
                continue

            # 解析视频直链
            detail_info = self.parse_detail_page(detail_html)

            if detail_info.get("video_url"):
                video["video_url"] = detail_info["video_url"]
                if detail_info.get("title"):
                    video["title"] = detail_info["title"]
                list_source_id = video.get("source_id", "")
                detail_source_id = detail_info.get("source_id", "")
                if list_source_id and detail_source_id and list_source_id != detail_source_id:
                    self.log(
                        f"  [FAIL] 详情页视频源不匹配: list_source_id={list_source_id} "
                        f"detail_source_id={detail_source_id} viewkey={video['viewkey']}"
                    )
                    self.failed_videos += 1
                    self.skip_viewkeys.add(video['viewkey'])
                    continue
                if not list_source_id and detail_source_id:
                    video["source_id"] = detail_source_id
                if video.get("source_id"):
                    video["thumb_url"] = self._thumb_url_for_source(
                        video.get("thumb_url", ""),
                        video["source_id"],
                    )
                    if video["source_id"] in self.skip_viewkeys:
                        self.log(f"  [SKIP] 已处理过 source_id: {video['source_id']}")
                        self.skipped_videos += 1
                        continue
                self.results.append(video)
                self.skip_viewkeys.add(video['viewkey'])
                if video.get("source_id"):
                    self.skip_viewkeys.add(video["source_id"])
                self.processed_videos += 1
                self.log(f"  [OK] 成功提取视频直链")
                # 流式：立刻把这条 entry 交给 Go 端开始下载，不等本批余下视频
                self.emit_stream_video(video)
            else:
                self.log(f"  [FAIL] 未找到视频直链: {video['viewkey']}")
                video["video_url"] = ""
                self.results.append(video)
                self.skip_viewkeys.add(video['viewkey'])
                self.failed_videos += 1

    def _save_results(self):
        """
        保存结果到JSON文件
        """
        output_data = {
            "crawl_time": datetime.now().isoformat(),
            "source_url": BASE_URL,
            "pages_crawled": self.pages_crawled,
            "total_videos": len(self.results),
            "successful": self.processed_videos,
            "skipped": self.skipped_videos,
            "failed": self.failed_videos,
            "videos": self.results
        }

        try:
            # 保证父目录存在；写入临时文件后原子 rename，避免读到半截 JSON
            out_path = self.output_file
            parent = os.path.dirname(os.path.abspath(out_path))
            if parent:
                os.makedirs(parent, exist_ok=True)
            tmp_path = out_path + ".part"
            with open(tmp_path, 'w', encoding='utf-8') as f:
                json.dump(output_data, f, ensure_ascii=False, indent=2)
            os.replace(tmp_path, out_path)
            self.log(f"结果已保存到: {os.path.abspath(out_path)}")
        except Exception as e:
            self.log(f"保存文件失败: {e}")
            # 尝试输出到控制台作为备份
            print("\n--- 备份输出 ---")
            print(json.dumps(output_data, ensure_ascii=False, indent=2))

    def _print_summary(self):
        """
        打印爬取摘要
        """
        self.log("")
        self.log("=" * 60)
        self.log("爬取完成!")
        self.log("=" * 60)
        self.log(f"爬取页数: {self.pages_crawled}")
        self.log(f"总视频数: {len(self.results)}")
        self.log(f"成功提取直链: {self.processed_videos}")
        self.log(f"跳过(已处理): {self.skipped_videos}")
        self.log(f"失败/缺失直链: {self.failed_videos}")
        self.log(f"输出文件: {os.path.abspath(self.output_file)}")
        self.log("=" * 60)


def print_help():
    print("""
================================================
    91porn 视频爬虫 v1.0
================================================

本脚本将爬取 91porn "本月最热" 分类下的所有视频信息：
  - 视频名称
  - 封面图直链
  - 视频直链 (MP4)

依赖安装:
    pip install requests beautifulsoup4 lxml

使用方法:
    python spider_91porn.py

配置说明 (编辑脚本内 "配置区域"):
    MIN_PAGE_DELAY / MAX_PAGE_DELAY : 列表页请求间隔 (默认 3-6 秒)
    MIN_DETAIL_DELAY / MAX_DETAIL_DELAY : 详情页请求间隔 (默认 2-5 秒)
    MAX_PAGES : 限制最大爬取页数 (None=不限, 如 5=只爬前5页)
    OUTPUT_FILE : 输出文件名 (默认 91porn_videos.json)

按 Ctrl+C 可随时中断并保存已爬取的数据

注意:
    1. 视频直链包含时效性token，会过期，需定期重新爬取
    2. 脚本已内置随机延时，请勿移除，避免对服务器造成压力
    3. 如遇到Cloudflare拦截，需要先通过浏览器获取Cookie
    4. 本脚本仅供学习交流，请遵守当地法律法规
================================================
""")


def main():
    if len(sys.argv) > 1 and sys.argv[1] in ('-h', '--help', 'help'):
        print_help()
        return

    parser = argparse.ArgumentParser(
        prog="spider_91porn.py",
        description="91porn 视频元数据爬虫",
        add_help=False,  # 让 -h/--help 走 print_help() 中文版本
    )
    parser.add_argument("--page", type=int, default=None,
                        help="只爬指定页（单页模式，配合 --output 用于定时任务）")
    parser.add_argument("--output", type=str, default=None,
                        help="输出 JSON 路径，覆盖默认 OUTPUT_FILE")
    parser.add_argument("--max-pages", type=int, default=None,
                        help="单页模式下，从 --page 起最多再爬几页（默认 1）")
    parser.add_argument("--no-resume", action="store_true",
                        help="禁用断点续爬（单页模式默认禁用）")
    parser.add_argument("--quiet", action="store_true",
                        help="压缩日志，每条视频只输出关键事件")
    parser.add_argument("--target-new", type=int, default=None,
                        help="目标新增模式：从 page 1 起翻页直到累计处理这么多新源视频后停止（backend 凌晨任务用）")
    parser.add_argument("--seen-viewkeys-file", type=str, default=None,
                        help="文件路径，每行一个已处理过的 viewkey 或 mp4 源 ID；脚本会跳过这些视频")
    parser.add_argument("--stream-output", action="store_true",
                        help="流式模式：每解析一条视频直链就立即把它作为一行 JSON 写到 stdout 并 flush；"
                             "日志改走 stderr。配合 backend 边读边下载使用。")

    args, _ = parser.parse_known_args()
    cli_out = sys.stderr if args.stream_output else sys.stdout
    prefer_ipv4_for_plain_socks5_proxy()

    print("""
================================================
    91porn 视频爬虫启动中...
================================================
按 Ctrl+C 可随时中断并保存进度
""", file=cli_out)

    # 加载已知 ID（来自 backend 的 catalog 已入库列表；兼容旧参数名）
    seen_viewkeys = []
    if args.seen_viewkeys_file:
        try:
            with open(args.seen_viewkeys_file, 'r', encoding='utf-8') as f:
                for line in f:
                    line = line.strip()
                    if line:
                        seen_viewkeys.append(line)
        except FileNotFoundError:
            print(f"警告: --seen-viewkeys-file 不存在: {args.seen_viewkeys_file}", file=cli_out)
        except Exception as e:
            print(f"警告: 读取 --seen-viewkeys-file 失败: {e}", file=cli_out)

    # 决定运行模式
    if args.target_new is not None:
        # 凑够 N 个新视频模式：从 page 1 起翻页，直到累计 target_new 个新视频
        spider = Porn91Spider(
            output_file=args.output,
            start_page=1,
            max_pages=None,
            resume=False,  # 凑够 N 模式靠 seen_viewkeys 去重，不读 OUTPUT_FILE
            quiet=args.quiet,
            target_new=args.target_new,
            seen_viewkeys=seen_viewkeys,
            stream_output=args.stream_output,
        )
    elif args.page is not None:
        # 单页模式（保留作手动调试用）：start_page=N, max_pages=1
        start_page = max(1, args.page)
        max_pages = args.max_pages if args.max_pages and args.max_pages > 0 else 1
        spider = Porn91Spider(
            output_file=args.output,
            start_page=start_page,
            max_pages=max_pages,
            resume=False,
            quiet=args.quiet,
            seen_viewkeys=seen_viewkeys,
            stream_output=args.stream_output,
        )
    else:
        # 全量模式（向后兼容）：从 page 1 起爬到末尾
        spider = Porn91Spider(
            output_file=args.output,
            resume=False if args.no_resume else None,
            quiet=args.quiet,
            seen_viewkeys=seen_viewkeys,
            stream_output=args.stream_output,
        )

    try:
        spider.crawl()
    except KeyboardInterrupt:
        spider.log("\n用户中断，正在保存已爬取的数据...")
        spider._save_results()
        spider._print_summary()
        sys.exit(0)
    except Exception as e:
        spider.log(f"发生未预料的错误: {e}")
        import traceback
        traceback.print_exc()
        spider._save_results()
        raise


if __name__ == "__main__":
    main()
