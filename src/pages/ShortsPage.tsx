import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  ChevronLeft,
  Heart,
  Maximize,
  Minimize,
  Volume2,
  VolumeX,
  Play,
  Pause,
  EyeOff,
  Info,
  Loader2,
  Sparkles,
  AlertCircle,
} from "lucide-react";
import {
  fetchShortsNext,
  hideVideo,
  type ShortsItem,
} from "@/data/videos";
import "@/styles/shorts.css";

// 短视频"已看过"列表存在 localStorage，与普通详情页历史完全独立。
const SEEN_STORAGE_KEY = "shorts_seen_ids_v1";

// 每次向后端取多少条续到队列尾。值不要太大避免一次返回过多浪费；
// 也不要太小导致频繁请求和滑动卡顿。
const BATCH_SIZE = 5;

// 当队列里"还没看过的视频"少于这个数时，提前请求下一批。
const PREFETCH_THRESHOLD = 2;

// 距离 activeIndex 多少屏内的视频会被 mount 真实 <video>。
// =1 表示上一屏 / 当前 / 下一屏 都加载，这样切换时几乎无空白。
const MOUNT_RADIUS = 1;

function loadSeenIds(): string[] {
  try {
    const raw = localStorage.getItem(SEEN_STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((x): x is string => typeof x === "string");
  } catch {
    return [];
  }
}

function saveSeenIds(ids: string[]) {
  try {
    localStorage.setItem(SEEN_STORAGE_KEY, JSON.stringify(ids));
  } catch {
    // 配额满或隐私模式：忽略，最多导致下一轮可能重复，不影响功能
  }
}

export default function ShortsPage() {
  // 已加入页面的视频队列（按出现顺序）
  const [items, setItems] = useState<ShortsItem[]>([]);
  // 当前在视口里的视频索引
  const [activeIndex, setActiveIndex] = useState(0);
  // 是否静音；首次必须静音才能 autoplay，用户点击后切换
  const [muted, setMuted] = useState(true);
  // 音量大小 (0 ~ 1)
  const [volume, setVolume] = useState(0.8);
  // 全局 Toast / HUD 提醒文字
  const [hudText, setHudText] = useState<{ id: number; text: string; icon?: React.ReactNode } | null>(null);
  const hudTimeoutRef = useRef<number | null>(null);

  const showHud = useCallback((text: string, icon?: React.ReactNode) => {
    if (hudTimeoutRef.current) window.clearTimeout(hudTimeoutRef.current);
    setHudText({ id: Date.now(), text, icon });
    hudTimeoutRef.current = window.setTimeout(() => {
      setHudText(null);
    }, 1500);
  }, []);

  const handleVolumeButtonClick = useCallback(() => {
    setMuted((v) => {
      const next = !v;
      showHud(
        next ? "已静音" : "音量已开启",
        next ? <VolumeX size={16} /> : <Volume2 size={16} />
      );
      return next;
    });
  }, [showHud]);

  const handleVolumeSliderChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const val = parseFloat(e.target.value);
    setVolume(val);
    if (val > 0) {
      setMuted(false);
    } else {
      setMuted(true);
    }
    // Update active video volume directly
    const activeVideo = videoRefs.current.get(activeIndex);
    if (activeVideo) {
      activeVideo.volume = val;
      activeVideo.muted = val === 0;
    }
  }, [activeIndex]);

  // 组件卸载时清理 HUD 定时器
  useEffect(() => {
    return () => {
      if (hudTimeoutRef.current) window.clearTimeout(hudTimeoutRef.current);
    };
  }, []);

  // 是否正在加载下一批，避免并发请求
  const [loading, setLoading] = useState(false);
  // 后端报告"本轮已耗尽"，下次请求前会自动重置
  const [roundComplete, setRoundComplete] = useState(false);
  // 没有任何视频可放（库为空 / 全部隐藏）
  const [empty, setEmpty] = useState(false);

  // seenIds 用 ref 维护，方便在异步 callback 里读到最新值
  const seenIdsRef = useRef<string[]>(loadSeenIds());

  const containerRef = useRef<HTMLDivElement | null>(null);
  // 整个页面根元素，用于 requestFullscreen
  const pageRef = useRef<HTMLDivElement | null>(null);
  // index → video element，用来精确控制播放/暂停
  const videoRefs = useRef<Map<number, HTMLVideoElement>>(new Map());

  // 当前是否处在浏览器全屏（Fullscreen API）状态。
  // iOS Safari 不支持元素级 Fullscreen API，这里会一直保持 false，
  // 全屏按钮在那种环境下点了也无效（按钮仍展示"进入全屏"图标）。
  const [isFullscreen, setIsFullscreen] = useState(false);
  // 自动尝试进入全屏只做一次，避免反复打扰用户
  const autoFullscreenAttemptedRef = useRef(false);

  // 本次会话内已经点过赞的视频 id 集合。
  // 与后端的真实 likes 字段同步——后端是单纯计数器，前端在这里防重避免连发。
  // 用户在操作栏点取消时会从这里移除，允许之后再次点赞。
  const likedIdsRef = useRef<Set<string>>(new Set());

  /**
   * 切换点赞状态。
   * - liked=true：发 POST /api/video/:id/like
   * - liked=false：发 DELETE /api/video/:id/like
   * 返回服务端最新 likes 值；请求失败返回 null（调用方可回滚 UI）。
   */
  const handleLikeToggle = useCallback(
    async (videoId: string, liked: boolean): Promise<number | null> => {
      // 维护本地集合以保持双击去重逻辑（已经在集合里就不会重复点赞）
      if (liked) {
        likedIdsRef.current.add(videoId);
      } else {
        likedIdsRef.current.delete(videoId);
      }
      try {
        const res = await fetch(
          `/api/video/${encodeURIComponent(videoId)}/like`,
          {
            method: liked ? "POST" : "DELETE",
            credentials: "include",
          }
        );
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = (await res.json()) as { likes?: number };
        return typeof data.likes === "number" ? data.likes : null;
      } catch {
        // 请求失败：回滚集合，让 Slide 自己回滚 UI
        if (liked) {
          likedIdsRef.current.delete(videoId);
        } else {
          likedIdsRef.current.add(videoId);
        }
        return null;
      }
    },
    []
  );

  /** 当前 id 是否已经在本次会话内点过赞（供 Slide 切换 active 时同步状态） */
  const hasLiked = useCallback(
    (videoId: string) => likedIdsRef.current.has(videoId),
    []
  );

  /**
   * 向后端请求下一批不重复的短视频，追加到 items 末尾。
   */
  const loadMore = useCallback(async () => {
    if (loading) return;
    setLoading(true);
    try {
      const seen = seenIdsRef.current;
      const resp = await fetchShortsNext(seen, BATCH_SIZE);
      if (resp.items.length === 0) {
        setEmpty((prev) => prev || true /* 维持 true 即可 */);
        setRoundComplete(true);
        return;
      }
      setEmpty(false);
      setItems((prev) => {
        const existing = new Set(prev.map((v) => v.id));
        const fresh = resp.items.filter((v) => !existing.has(v.id));
        return [...prev, ...fresh];
      });
      setRoundComplete(resp.roundComplete);
    } finally {
      setLoading(false);
    }
  }, [loading]);

  // 首次加载
  useEffect(() => {
    void loadMore();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 仅当 items 还是空时，把 empty 设回 false 是没必要的；上面 loadMore 已处理
  useEffect(() => {
    if (items.length > 0) setEmpty(false);
  }, [items.length]);

  // 把当前活跃视频的 id 写入已看列表，并在剩余不足时续取
  useEffect(() => {
    const active = items[activeIndex];
    if (!active) return;

    if (!seenIdsRef.current.includes(active.id)) {
      seenIdsRef.current = [...seenIdsRef.current, active.id];
      saveSeenIds(seenIdsRef.current);
    }

    const remaining = items.length - 1 - activeIndex;
    if (remaining < PREFETCH_THRESHOLD && !loading) {
      if (roundComplete) {
        // 上一次后端说"本轮已耗尽"，且当前已经看到队列接近末尾。
        // 清空 localStorage 后再请求即可开新一轮。
        seenIdsRef.current = [];
        saveSeenIds([]);
        setRoundComplete(false);
      }
      void loadMore();
    }
  }, [activeIndex, items, loading, roundComplete, loadMore]);

  // 用 IntersectionObserver 找出当前进入视口的 item
  useEffect(() => {
    const root = containerRef.current;
    if (!root) return;

    const observer = new IntersectionObserver(
      (entries) => {
        let bestIndex = -1;
        let bestRatio = 0.6;
        for (const entry of entries) {
          if (entry.intersectionRatio > bestRatio) {
            bestRatio = entry.intersectionRatio;
            const idx = Number(
              (entry.target as HTMLElement).dataset.index ?? -1
            );
            if (!Number.isNaN(idx)) bestIndex = idx;
          }
        }
        if (bestIndex >= 0) setActiveIndex(bestIndex);
      },
      {
        root,
        threshold: [0.6, 0.85],
      }
    );

    const slides = root.querySelectorAll<HTMLElement>("[data-shorts-slide]");
    slides.forEach((el) => observer.observe(el));
    return () => observer.disconnect();
  }, [items.length]);

  // 控制每个 video 的播放状态与音量：只有 activeIndex 对应的在播
  useEffect(() => {
    videoRefs.current.forEach((video, idx) => {
      if (idx === activeIndex) {
        video.muted = muted;
        video.volume = volume;
        if (video.paused) {
          // 切到这个视频时从头开始播
          try {
            video.currentTime = 0;
          } catch {
            // ignore
          }
          video.play().catch(() => undefined);
        }
      } else {
        if (!video.paused) video.pause();
        try {
          video.currentTime = 0;
        } catch {
          // ignore
        }
      }
    });
  }, [activeIndex, muted, volume, items.length]);

  // 键盘快捷键监听
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      const activeEl = document.activeElement;
      if (activeEl && (activeEl.tagName === "INPUT" || activeEl.tagName === "TEXTAREA")) {
        return;
      }

      if (e.key === "ArrowDown") {
        e.preventDefault();
        const nextIdx = activeIndex + 1;
        if (nextIdx < items.length) {
          const nextSlide = containerRef.current?.querySelector(`[data-index="${nextIdx}"]`);
          if (nextSlide) {
            nextSlide.scrollIntoView({ behavior: "smooth" });
          }
        }
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        const prevIdx = activeIndex - 1;
        if (prevIdx >= 0) {
          const prevSlide = containerRef.current?.querySelector(`[data-index="${prevIdx}"]`);
          if (prevSlide) {
            prevSlide.scrollIntoView({ behavior: "smooth" });
          }
        }
      } else if (e.key === " ") {
        e.preventDefault();
        const activeVideo = videoRefs.current.get(activeIndex);
        if (activeVideo) {
          if (activeVideo.paused) {
            activeVideo.play().catch(() => undefined);
            showHud("播放", <Play size={16} fill="currentColor" />);
          } else {
            activeVideo.pause();
            showHud("暂停", <Pause size={16} fill="currentColor" />);
          }
        }
      } else if (e.key === "m" || e.key === "M") {
        e.preventDefault();
        handleVolumeButtonClick();
      } else if (e.key === "f" || e.key === "F") {
        e.preventDefault();
        toggleFullscreen();
      } else if (e.key === "l" || e.key === "L") {
        e.preventDefault();
        const heartBtn = containerRef.current?.querySelector(`[data-index="${activeIndex}"] .shorts-slide__action`) as HTMLButtonElement | null;
        if (heartBtn) {
          heartBtn.click();
        }
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        const activeVideo = videoRefs.current.get(activeIndex);
        if (activeVideo && activeVideo.duration) {
          const newTime = Math.min(activeVideo.duration, activeVideo.currentTime + 5);
          activeVideo.currentTime = newTime;
          showHud("+5秒", <Sparkles size={16} />);
        }
      } else if (e.key === "ArrowLeft") {
        e.preventDefault();
        const activeVideo = videoRefs.current.get(activeIndex);
        if (activeVideo && activeVideo.duration) {
          const newTime = Math.max(0, activeVideo.currentTime - 5);
          activeVideo.currentTime = newTime;
          showHud("-5秒", <Sparkles size={16} />);
        }
      }
    };

    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [activeIndex, items, toggleFullscreen, showHud, handleVolumeButtonClick]);

  // 页面卸载时暂停所有
  useEffect(() => {
    return () => {
      videoRefs.current.forEach((v) => {
        try {
          v.pause();
        } catch {
          // ignore
        }
      });
    };
  }, []);

  const setVideoRef = useCallback(
    (index: number) => (el: HTMLVideoElement | null) => {
      if (el) videoRefs.current.set(index, el);
      else videoRefs.current.delete(index);
    },
    []
  );

  useEffect(() => {
    document.title = "短视频 · 91";
  }, []);

  // 沉浸式：进入页面后锁住 body 滚动 + 把主题色改黑（Android Chrome 状态栏会变黑）
  useEffect(() => {
    const html = document.documentElement;
    const body = document.body;
    const prevHtmlOverflow = html.style.overflow;
    const prevBodyOverflow = body.style.overflow;
    const prevBodyBg = body.style.background;
    html.style.overflow = "hidden";
    body.style.overflow = "hidden";
    body.style.background = "#000";

    let prevThemeColor: string | null = null;
    let themeMeta = document.querySelector<HTMLMetaElement>(
      'meta[name="theme-color"]'
    );
    const createdMeta = !themeMeta;
    if (!themeMeta) {
      themeMeta = document.createElement("meta");
      themeMeta.name = "theme-color";
      document.head.appendChild(themeMeta);
    } else {
      prevThemeColor = themeMeta.content;
    }
    themeMeta.content = "#000000";

    return () => {
      html.style.overflow = prevHtmlOverflow;
      body.style.overflow = prevBodyOverflow;
      body.style.background = prevBodyBg;
      if (themeMeta) {
        if (createdMeta) {
          themeMeta.remove();
        } else if (prevThemeColor !== null) {
          themeMeta.content = prevThemeColor;
        }
      }
    };
  }, []);

  // ---- 浏览器全屏（Fullscreen API） ----
  // 监听全屏状态变化，保持 React state 同步。
  // 用户按 ESC / 系统返回 / 浏览器退出全屏按钮 时也会走这里。
  useEffect(() => {
    function handleChange() {
      setIsFullscreen(
        document.fullscreenElement !== null ||
          // Safari (desktop) 旧前缀
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          (document as any).webkitFullscreenElement != null
      );
    }
    document.addEventListener("fullscreenchange", handleChange);
    document.addEventListener("webkitfullscreenchange", handleChange);
    return () => {
      document.removeEventListener("fullscreenchange", handleChange);
      document.removeEventListener("webkitfullscreenchange", handleChange);
    };
  }, []);

  // 路由离开 / 组件卸载时主动退出全屏，避免残留全屏态
  useEffect(() => {
    return () => {
      try {
        if (document.fullscreenElement) {
          void document.exitFullscreen();
        }
      } catch {
        // ignore
      }
    };
  }, []);

  // 进入页面后第一次任意触摸时尝试自动进入全屏。
  // 浏览器要求 requestFullscreen 必须在用户手势内调用；进页面时直接调
  // 一定会被拒绝，所以挂在 pointerdown 上利用第一次手势。
  // iOS Safari 不支持元素级 Fullscreen API，这里 catch 后保持原样，
  // 退化为已经做的 100svh 沉浸样式。
  useEffect(() => {
    const page = pageRef.current;
    if (!page) return;
    function onFirstPointer() {
      if (autoFullscreenAttemptedRef.current) return;
      autoFullscreenAttemptedRef.current = true;
      requestPageFullscreen();
    }
    page.addEventListener("pointerdown", onFirstPointer, {
      once: true,
      passive: true,
    });
    return () => {
      page.removeEventListener("pointerdown", onFirstPointer);
    };
  }, []);

  function requestPageFullscreen() {
    const page = pageRef.current;
    if (!page) return;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const anyPage = page as any;
    const fn: (() => Promise<void>) | undefined =
      page.requestFullscreen?.bind(page) ||
      anyPage.webkitRequestFullscreen?.bind(page);
    if (!fn) return;
    try {
      const ret = fn();
      if (ret && typeof ret.then === "function") {
        ret.catch(() => {
          // iOS Safari 或被拒绝：静默忽略，沉浸样式仍然生效
        });
      }
    } catch {
      // ignore
    }
  }

  function exitPageFullscreen() {
    try {
      if (document.exitFullscreen) {
        void document.exitFullscreen();
      } else {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        const anyDoc = document as any;
        if (typeof anyDoc.webkitExitFullscreen === "function") {
          anyDoc.webkitExitFullscreen();
        }
      }
    } catch {
      // ignore
    }
  }

  function toggleFullscreen() {
    if (isFullscreen) exitPageFullscreen();
    else requestPageFullscreen();
  }

  const handleHideSuccess = useCallback((idx: number) => {
    showHud("已选择不再展示，正在滑至下一首...", <EyeOff size={16} />);
    const nextIdx = idx + 1;
    if (nextIdx < items.length) {
      setTimeout(() => {
        const nextSlide = containerRef.current?.querySelector(`[data-index="${nextIdx}"]`);
        if (nextSlide) {
          nextSlide.scrollIntoView({ behavior: "smooth" });
        }
      }, 700);
    }
  }, [items.length, showHud]);

  return (
    <div className="shorts-page" ref={pageRef}>
      <header className="shorts-header">
        <Link to="/" className="shorts-header__back" aria-label="返回首页">
          <ChevronLeft size={22} />
        </Link>
        <div className="shorts-header__actions">
          <button
            type="button"
            className="shorts-header__icon-btn"
            aria-label={isFullscreen ? "退出全屏" : "进入全屏"}
            onClick={toggleFullscreen}
          >
            {isFullscreen ? <Minimize size={20} /> : <Maximize size={20} />}
          </button>
          
          <div className="shorts-header__volume-group">
            <div className="shorts-header__volume-slider-container">
              <input
                type="range"
                min="0"
                max="1"
                step="0.05"
                value={muted ? 0 : volume}
                onChange={handleVolumeSliderChange}
                className="shorts-header__volume-slider"
                aria-label="音量调节"
              />
            </div>
            <button
              type="button"
              className="shorts-header__icon-btn"
              aria-label={muted ? "取消静音" : "静音"}
              onClick={handleVolumeButtonClick}
            >
              {muted || volume === 0 ? <VolumeX size={20} /> : <Volume2 size={20} />}
            </button>
          </div>
        </div>
      </header>

      {hudText && (
        <div key={hudText.id} className="shorts-hud-toast">
          {hudText.icon}
          <span>{hudText.text}</span>
        </div>
      )}

      <div className="shorts-feed" ref={containerRef}>
        {empty && (
          <div className="shorts-empty">
            <p>当前没有可播放的视频</p>
            <Link to="/" className="shorts-empty__link">
              返回首页
            </Link>
          </div>
        )}

        {items.map((item, index) => (
          <ShortsSlide
            key={item.id}
            item={item}
            index={index}
            isActive={index === activeIndex}
            // 距离 active 在 MOUNT_RADIUS 之内才挂载真正的 <video>，
            // 其它槽位用海报占位以节省内存和带宽
            shouldMount={Math.abs(index - activeIndex) <= MOUNT_RADIUS}
            muted={muted}
            volume={volume}
            setMuted={setMuted}
            setVolume={setVolume}
            videoRef={setVideoRef(index)}
            onLikeToggle={handleLikeToggle}
            hasLiked={hasLiked}
            onHideSuccess={handleHideSuccess}
            showHud={showHud}
          />
        ))}

        {!empty && items.length > 0 && loading && (
          <div className="shorts-loading">
            <Loader2 size={16} className="shorts-slide__buffering-icon" />
            <span>加载中…</span>
          </div>
        )}
      </div>
    </div>
  );
}

type SlideProps = {
  item: ShortsItem;
  index: number;
  isActive: boolean;
  shouldMount: boolean;
  muted: boolean;
  volume: number;
  setMuted: (muted: boolean) => void;
  setVolume: (volume: number) => void;
  videoRef: (el: HTMLVideoElement | null) => void;
  /**
   * 切换点赞。第二参数 true 表示点赞，false 表示取消。
   * 返回服务端最新 likes 值；null 表示请求失败，调用方应回滚 UI。
   */
  onLikeToggle: (videoId: string, liked: boolean) => Promise<number | null>;
  /** 父组件查询某 id 是否已经在本次会话内点过赞 */
  hasLiked: (videoId: string) => boolean;
  onHideSuccess: (index: number) => void;
  showHud: (text: string, icon?: React.ReactNode) => void;
};

/**
 * 一屏短视频。
 *
 * - 长按 ≥400ms 进入 2 倍速，松手恢复（与详情页 VideoPlayer 行为一致）
 * - 单击切换播放 / 暂停
 * - 长按弹出的下载/分享菜单通过 contextmenu + CSS 屏蔽
 */
function ShortsSlide({
  item,
  index,
  isActive,
  shouldMount,
  muted,
  volume,
  setMuted,
  setVolume,
  videoRef,
  onLikeToggle,
  hasLiked,
  onHideSuccess,
  showHud,
}: SlideProps) {
  const localRef = useRef<HTMLVideoElement | null>(null);
  const [paused, setPaused] = useState(false);
  const [fastActive, setFastActive] = useState(false);

  // 视频缓冲状态
  const [isBuffering, setIsBuffering] = useState(false);
  // 单击播放暂停的瞬间 HUD 动效
  const [playPauseHud, setPlayPauseHud] = useState<{ id: number; type: "play" | "pause" } | null>(null);
  // 是否已经被隐藏/拉黑
  const [isMarkedHidden, setIsMarkedHidden] = useState(false);

  // 进度状态。播放时由 timeupdate 更新；拖动时由用户输入更新
  const [duration, setDuration] = useState(0);
  const [currentTime, setCurrentTime] = useState(0);
  const [scrubbing, setScrubbing] = useState(false);
  // 拖动开始时是否在播：用于拖完后判断要不要 resume
  const wasPlayingRef = useRef(true);

  // 点赞数和"是否已点过赞"状态。
  // 初始 likes 取自后端返回的列表项；isLiked 仅控制视觉态，
  // 真正的防重在父组件 likedIdsRef 里，这里只信任父返回的回执。
  const [likes, setLikes] = useState(item.likes ?? 0);
  const [isLiked, setIsLiked] = useState(false);
  // 屏幕中央的心形飞起动画（双击点赞时显示）
  const [heartBurst, setHeartBurst] = useState<{
    key: number;
    x: number;
    y: number;
  } | null>(null);

  // 单击和双击的延迟分发：第一次点击挂在定时器里，
  // 300ms 内有第二次就当双击点赞，否则当单击 toggle play
  const clickTimerRef = useRef<number | null>(null);
  const lastClickAtRef = useRef(0);

  // 切换视频时把 likes 同步到新视频的初始值；
  // isLiked 取自父组件的全局集合，这样切走再切回 / 同一 id 重复出现仍能保持视觉态
  useEffect(() => {
    setLikes(item.likes ?? 0);
    setIsLiked(hasLiked(item.id));
  }, [item.id, item.likes, hasLiked]);

  const setRef = useCallback(
    (el: HTMLVideoElement | null) => {
      localRef.current = el;
      videoRef(el);
    },
    [videoRef]
  );

  // 离开活跃后清掉本地的暂停状态，避免回来时 UI 还显示着 paused
  useEffect(() => {
    if (!isActive) {
      setPaused(false);
      setScrubbing(false);
      setIsBuffering(false);
      setPlayPauseHud(null);
    }
  }, [isActive]);

  // Sync volume state directly
  useEffect(() => {
    const video = localRef.current;
    if (video && isActive) {
      video.muted = muted;
      video.volume = volume;
    }
  }, [muted, volume, isActive]);

  // 离开活跃或者被隐藏时暂停视频
  useEffect(() => {
    if (isMarkedHidden && localRef.current) {
      try {
        localRef.current.pause();
      } catch {
        // ignore
      }
    }
  }, [isMarkedHidden]);

  // 监听 video 的时长 / 进度 / 缓冲状态 / 音量物理键变化
  useEffect(() => {
    const video = localRef.current;
    if (!video) return;
    const handleLoaded = () => {
      if (Number.isFinite(video.duration)) setDuration(video.duration);
    };
    const handleTime = () => {
      // 拖动期间不要被 timeupdate 覆盖 UI
      if (!scrubbing) setCurrentTime(video.currentTime);
    };
    const handleWaiting = () => {
      setIsBuffering(true);
    };
    const handlePlayingOrCanPlay = () => {
      setIsBuffering(false);
    };
    const handleVolumeChange = () => {
      // 当检测到 video 自身的 mute 状态或 volume 改变时，同步更新 React 状态。
      // 这可以在移动端浏览器支持物理音量键调整时，自动反向取消静音并展示音量 HUD。
      if (video.muted !== muted) {
        setMuted(video.muted);
      }
      if (video.volume !== volume) {
        setVolume(video.volume);
      }
    };

    handleLoaded();
    video.addEventListener("loadedmetadata", handleLoaded);
    video.addEventListener("durationchange", handleLoaded);
    video.addEventListener("timeupdate", handleTime);
    video.addEventListener("waiting", handleWaiting);
    video.addEventListener("playing", handlePlayingOrCanPlay);
    video.addEventListener("canplay", handlePlayingOrCanPlay);
    video.addEventListener("volumechange", handleVolumeChange);

    // 挂载时如果已经在播放但是状态不到 ready 则置 buffering
    if (video.readyState < 3 && !video.paused) {
      setIsBuffering(true);
    }

    return () => {
      video.removeEventListener("loadedmetadata", handleLoaded);
      video.removeEventListener("durationchange", handleLoaded);
      video.removeEventListener("timeupdate", handleTime);
      video.removeEventListener("waiting", handleWaiting);
      video.removeEventListener("playing", handlePlayingOrCanPlay);
      video.removeEventListener("canplay", handlePlayingOrCanPlay);
      video.removeEventListener("volumechange", handleVolumeChange);
    };
  }, [shouldMount, scrubbing, muted, volume, setMuted, setVolume]);

  // 长按 2 倍速：直接绑原生事件
  useEffect(() => {
    const video = localRef.current;
    if (!video) return;
    let timer: number | null = null;
    let active = false;

    const clearTimer = () => {
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    };
    const start = () => {
      if (video.paused || video.ended) return;
      clearTimer();
      timer = window.setTimeout(() => {
        timer = null;
        if (video.paused || video.ended) return;
        video.playbackRate = 2;
        active = true;
        setFastActive(true);
      }, 400);
    };
    const end = () => {
      clearTimer();
      if (active) {
        active = false;
        video.playbackRate = 1;
        setFastActive(false);
      }
    };

    const handleTouchStart = () => start();
    const handleMouseDown = (e: MouseEvent) => {
      if (e.button === 0) start();
    };

    video.addEventListener("touchstart", handleTouchStart, { passive: true });
    video.addEventListener("touchend", end);
    video.addEventListener("touchcancel", end);
    video.addEventListener("mousedown", handleMouseDown);
    video.addEventListener("mouseup", end);
    video.addEventListener("mouseleave", end);
    video.addEventListener("pause", end);
    video.addEventListener("ended", end);

    return () => {
      clearTimer();
      video.removeEventListener("touchstart", handleTouchStart);
      video.removeEventListener("touchend", end);
      video.removeEventListener("touchcancel", end);
      video.removeEventListener("mousedown", handleMouseDown);
      video.removeEventListener("mouseup", end);
      video.removeEventListener("mouseleave", end);
      video.removeEventListener("pause", end);
      video.removeEventListener("ended", end);
    };
  }, [shouldMount]);

  function togglePlayInternal() {
    const video = localRef.current;
    if (!video) return;
    if (video.paused) {
      video.play().catch(() => undefined);
      setPaused(false);
      setPlayPauseHud({ id: Date.now(), type: "play" });
      setTimeout(() => setPlayPauseHud(null), 450);
    } else {
      video.pause();
      setPaused(true);
      setPlayPauseHud({ id: Date.now(), type: "pause" });
      setTimeout(() => setPlayPauseHud(null), 450);
    }
  }

  function clearClickTimer() {
    if (clickTimerRef.current !== null) {
      window.clearTimeout(clickTimerRef.current);
      clickTimerRef.current = null;
    }
  }

  /**
   * 单击 / 双击分发：
   * - 第一次点击：挂一个 280ms 定时器，到时如果还没第二次点击就 toggle 播放
   * - 第二次点击（280ms 内）：清掉定时器，当作双击点赞，不切换播放状态
   */
  function handleSlideClick(e: React.MouseEvent<HTMLElement>) {
    // 隐藏状态下不处理点击
    if (isMarkedHidden) return;

    const now = Date.now();
    const delta = now - lastClickAtRef.current;
    lastClickAtRef.current = now;

    // 双击命中
    if (delta < 280 && clickTimerRef.current !== null) {
      clearClickTimer();
      // 在双击位置弹心形动画
      const rect = e.currentTarget.getBoundingClientRect();
      handleDoubleClickLike(e.clientX - rect.left, e.clientY - rect.top);
      return;
    }

    // 单击挂起，等是否有第二次
    clearClickTimer();
    clickTimerRef.current = window.setTimeout(() => {
      clickTimerRef.current = null;
      togglePlayInternal();
    }, 280);
  }

  // 组件卸载时清理定时器
  useEffect(() => {
    return () => clearClickTimer();
  }, []);

  function handleDoubleClickLike(x: number, y: number) {
    // 触发飞心动画（每次都给一个新 key 强制重启动画）
    setHeartBurst({ key: Date.now(), x, y });
    window.setTimeout(() => setHeartBurst(null), 700);

    // 双击只表达喜爱：已经点赞了就只播动画不取消，不重复发请求；
    // 真要取消请点右下角心形按钮
    if (isLiked) return;
    setIsLiked(true);
    setLikes((n) => n + 1);
    void onLikeToggle(item.id, true).then((serverLikes) => {
      if (serverLikes !== null) {
        setLikes(serverLikes);
      } else {
        // 请求失败：回滚视觉态
        setIsLiked(false);
        setLikes((n) => Math.max(0, n - 1));
      }
    });
  }

  /**
   * 点击右下角心形按钮：在"已点赞 / 未点赞"之间切换。
   */
  function handleHeartClick(e: React.MouseEvent<HTMLButtonElement>) {
    e.stopPropagation();
    const willLike = !isLiked;
    if (willLike) {
      // 视觉立即响应 + 飞心动画（让按钮位置发出心形）
      const slideRect = (
        e.currentTarget.closest(".shorts-slide") as HTMLElement | null
      )?.getBoundingClientRect();
      const btnRect = e.currentTarget.getBoundingClientRect();
      if (slideRect) {
        const x = btnRect.left + btnRect.width / 2 - slideRect.left;
        const y = btnRect.top + btnRect.height / 2 - slideRect.top;
        setHeartBurst({ key: Date.now(), x, y });
        window.setTimeout(() => setHeartBurst(null), 700);
      }
      setIsLiked(true);
      setLikes((n) => n + 1);
      void onLikeToggle(item.id, true).then((serverLikes) => {
        if (serverLikes !== null) {
          setLikes(serverLikes);
        } else {
          setIsLiked(false);
          setLikes((n) => Math.max(0, n - 1));
        }
      });
    } else {
      // 取消点赞：视觉立即响应，请求失败再回滚
      setIsLiked(false);
      setLikes((n) => Math.max(0, n - 1));
      void onLikeToggle(item.id, false).then((serverLikes) => {
        if (serverLikes !== null) {
          setLikes(serverLikes);
        } else {
          setIsLiked(true);
          setLikes((n) => n + 1);
        }
      });
    }
  }



  /**
   * 拉黑并隐藏视频
   */
  function handleHideClick(e: React.MouseEvent<HTMLButtonElement>) {
    e.stopPropagation();
    setIsMarkedHidden(true);
    void hideVideo(item.id)
      .then((res) => {
        if (res.ok) {
          onHideSuccess(index);
        } else {
          setIsMarkedHidden(false);
          showHud("操作失败，请重试", <AlertCircle size={16} />);
        }
      })
      .catch(() => {
        setIsMarkedHidden(false);
        showHud("网络请求出错", <AlertCircle size={16} />);
      });
  }

  // ---- 进度条拖动 ----
  // 触摸进度条时：暂停 → 跟随手指更新 currentTime → 松手 resume
  function handleProgressPointerDown(e: React.PointerEvent<HTMLDivElement>) {
    const video = localRef.current;
    if (!video || !duration) return;
    e.preventDefault();
    e.stopPropagation();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    wasPlayingRef.current = !video.paused;
    if (!video.paused) {
      try {
        video.pause();
      } catch {
        // ignore
      }
    }
    setScrubbing(true);
    applyProgressFromEvent(e);
  }
  function handleProgressPointerMove(e: React.PointerEvent<HTMLDivElement>) {
    if (!scrubbing) return;
    e.preventDefault();
    e.stopPropagation();
    applyProgressFromEvent(e);
  }
  function handleProgressPointerEnd(e: React.PointerEvent<HTMLDivElement>) {
    if (!scrubbing) return;
    e.stopPropagation();
    try {
      (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    } catch {
      // ignore
    }
    const video = localRef.current;
    setScrubbing(false);
    if (video && wasPlayingRef.current) {
      video.play().catch(() => undefined);
    }
  }
  function applyProgressFromEvent(e: React.PointerEvent<HTMLDivElement>) {
    const video = localRef.current;
    if (!video || !duration) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const ratio = clamp((e.clientX - rect.left) / rect.width, 0, 1);
    const next = ratio * duration;
    setCurrentTime(next);
    try {
      video.currentTime = next;
    } catch {
      // ignore（部分 ready state 下设置会抛错）
    }
  }

  const progressRatio = duration > 0 ? clamp(currentTime / duration, 0, 1) : 0;

  return (
    <article
      className="shorts-slide"
      data-shorts-slide=""
      data-index={index}
      data-active={isActive}
      onClick={handleSlideClick}
    >
      {/* 模糊海报背景：避免横屏视频两边出现刺眼黑边 */}
      <div
        className="shorts-slide__bg"
        style={{ backgroundImage: `url(${item.poster})` }}
        aria-hidden="true"
      />

      {shouldMount ? (
        <video
          ref={setRef}
          className="shorts-slide__video"
          src={item.videoSrc}
          poster={item.poster}
          preload="auto"
          playsInline
          loop
          muted={muted}
          controlsList="nodownload"
          disablePictureInPicture
          onContextMenu={(e) => e.preventDefault()}
        />
      ) : (
        <img
          className="shorts-slide__poster"
          src={item.poster}
          alt=""
          aria-hidden="true"
          loading="lazy"
        />
      )}

      {fastActive && (
        <div className="shorts-slide__rate-hint" aria-hidden="true">
          2x 速播放中
        </div>
      )}



      {paused && isActive && !scrubbing && !playPauseHud && (
        <div className="shorts-slide__paused" aria-hidden="true">
          ▶
        </div>
      )}

      {/* 视频加载/缓冲旋转器 */}
      {isBuffering && isActive && shouldMount && !isMarkedHidden && (
        <div className="shorts-slide__buffering" aria-hidden="true">
          <Loader2 size={30} className="shorts-slide__buffering-icon" />
        </div>
      )}

      {/* 播放暂停瞬间 HUD 动效 */}
      {playPauseHud && isActive && (
        <div key={playPauseHud.id} className="shorts-slide__hud-pulse" aria-hidden="true">
          {playPauseHud.type === "play" ? <Play size={30} fill="currentColor" /> : <Pause size={30} fill="currentColor" />}
        </div>
      )}

      {/* 不再展示屏蔽遮罩 */}
      {isMarkedHidden && (
        <div className="shorts-slide__hidden-overlay" onClick={(e) => e.stopPropagation()}>
          <EyeOff size={38} style={{ color: "#ff4060", marginBottom: "8px" }} />
          <div className="shorts-slide__hidden-title">已隐藏该视频</div>
          <div className="shorts-slide__hidden-desc">系统将不会再次在任何地方向您展示此视频</div>
        </div>
      )}

      <div className="shorts-slide__overlay" onClick={(e) => e.stopPropagation()}>
        <h2 className="shorts-slide__title">{item.title}</h2>
        <div className="shorts-slide__meta">
          {item.sourceLabel && (
            <span className="shorts-slide__meta-item">{item.sourceLabel}</span>
          )}
          {item.duration && (
            <span className="shorts-slide__meta-item">{item.duration}</span>
          )}
          {item.tags && item.tags.length > 0 && (
            <span className="shorts-slide__meta-item">
              {item.tags.slice(0, 3).map((t) => `#${t}`).join(" ")}
            </span>
          )}
        </div>
        <Link
          to={`/video/${encodeURIComponent(item.id)}`}
          className="shorts-slide__detail"
        >
          <Info size={13} />
          <span>查看详情</span>
        </Link>
      </div>

      {/* 右下角操作栏 */}
      <aside
        className="shorts-slide__actions"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 云盘来源徽章 */}
        <div className="shorts-drive-badge" title={`来源: ${item.sourceLabel || "本地"}`}>
          {getDriveShortName(item.sourceLabel || "本地")}
        </div>

        {/* 点赞 */}
        <button
          type="button"
          className={`shorts-slide__action ${isLiked ? "is-liked" : ""}`}
          aria-label={isLiked ? "取消点赞" : "点赞"}
          aria-pressed={isLiked}
          onClick={handleHeartClick}
        >
          <Heart
            size={24}
            fill={isLiked ? "currentColor" : "none"}
            strokeWidth={2}
          />
          <span className="shorts-slide__action-count">{formatCount(likes)}</span>
        </button>



        {/* 不再展示 */}
        <button
          type="button"
          className="shorts-slide__action"
          aria-label="不再展示"
          onClick={handleHideClick}
        >
          <EyeOff size={22} />
          <span className="shorts-slide__action-count">隐藏</span>
        </button>
      </aside>

      {/* 双击点赞时弹起的心形动画 */}
      {heartBurst && (
        <div
          key={heartBurst.key}
          className="shorts-slide__heart-burst"
          style={{ left: heartBurst.x, top: heartBurst.y }}
          aria-hidden="true"
        >
          <Heart size={88} fill="currentColor" strokeWidth={0} />
        </div>
      )}

      {/* 进度条 */}
      {shouldMount && !isMarkedHidden && (
        <div
          className={`shorts-slide__progress ${
            scrubbing ? "is-scrubbing" : ""
          }`}
          onPointerDown={handleProgressPointerDown}
          onPointerMove={handleProgressPointerMove}
          onPointerUp={handleProgressPointerEnd}
          onPointerCancel={handleProgressPointerEnd}
          onClick={(e) => e.stopPropagation()}
        >
          <div
            className="shorts-slide__progress-track"
            style={{
              "--progress-pct": `${progressRatio * 100}%`,
            } as React.CSSProperties}
          >
            <div
              className="shorts-slide__progress-fill"
              style={{ width: `${progressRatio * 100}%` }}
            />
          </div>
          {scrubbing && (
            <div className="shorts-slide__progress-time">
              {formatClock(currentTime)} / {formatClock(duration)}
            </div>
          )}
        </div>
      )}
    </article>
  );
}

function clamp(n: number, min: number, max: number) {
  return n < min ? min : n > max ? max : n;
}

function formatClock(seconds: number) {
  if (!Number.isFinite(seconds) || seconds < 0) return "00:00";
  const total = Math.floor(seconds);
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

/** 简易的点赞数缩写：1.2k / 3.4w，避免 5 位数挤爆右侧操作栏 */
function formatCount(n: number) {
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n < 1000) return String(n);
  if (n < 10000) return (n / 1000).toFixed(1).replace(/\.0$/, "") + "k";
  return (n / 10000).toFixed(1).replace(/\.0$/, "") + "w";
}

/** 识别云盘缩写名称 */
function getDriveShortName(source: string): string {
  const s = source.toLowerCase();
  if (s.includes("115")) return "115";
  if (s.includes("pikpak")) return "PikP";
  if (s.includes("quark") || s.includes("夸克")) return "Quak";
  if (s.includes("onedrive")) return "OneDrive";
  if (s.includes("wopan") || s.includes("沃盘")) return "沃盘";
  if (s.includes("localstorage") || s.includes("本地")) return "本地";
  if (s.includes("spider") || s.includes("爬虫")) return "爬虫";
  return source.substring(0, 4);
}
