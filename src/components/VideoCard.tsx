import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { Link } from "react-router-dom";
import type { PreviewState, VideoItem } from "@/types";
import { previewController } from "@/lib/previewController";
import {
  shouldInterceptPreviewTap,
  shouldStartInstantPreview,
} from "@/lib/previewIntent";
import { useInViewport } from "@/lib/useInViewport";
import { formatCount } from "@/lib/format";
import { PreviewVideo } from "./PreviewVideo";

type Props = {
  video: VideoItem;
};

const HOVER_DELAY_MS = 300;

function useActivePreviewId(): string | null {
  return useSyncExternalStore(
    previewController.subscribe,
    previewController.getActiveId,
    () => null
  );
}

export function VideoCard({ video }: Props) {
  const [previewState, setPreviewState] = useState<PreviewState>("idle");
  const [shouldRenderPreview, setShouldRenderPreview] = useState(false);
  const [progress, setProgress] = useState(0); // 0~1
  const [thumbnailRetry, setThumbnailRetry] = useState(0);

  const rootRef = useRef<HTMLElement | null>(null);
  const hoverTimerRef = useRef<number | null>(null);
  const thumbnailRetryTimerRef = useRef<number | null>(null);
  const lastPointerTypeRef = useRef<string>("");
  const canHoverRef = useRef(true);
  const videoRef = useRef<HTMLVideoElement | null>(null);

  const activeId = useActivePreviewId();
  const inView = useInViewport(rootRef);

  // 当全局活跃卡片不是自己时，立刻停止预览
  useEffect(() => {
    if (activeId !== video.id && shouldRenderPreview) {
      cleanup();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeId, video.id]);

  // 离开视口时停止预览
  useEffect(() => {
    if (!inView && shouldRenderPreview) {
      cleanup();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inView]);

  // 卸载时清理
  useEffect(() => {
    return () => {
      cleanup();
      if (thumbnailRetryTimerRef.current) {
        window.clearTimeout(thumbnailRetryTimerRef.current);
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const media = window.matchMedia("(hover: hover) and (pointer: fine)");
    const update = () => {
      canHoverRef.current = media.matches;
    };
    update();
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);

  useEffect(() => {
    setThumbnailRetry(0);
    if (thumbnailRetryTimerRef.current) {
      window.clearTimeout(thumbnailRetryTimerRef.current);
      thumbnailRetryTimerRef.current = null;
    }
  }, [video.id, video.thumbnail]);

  function cleanup() {
    if (hoverTimerRef.current) {
      window.clearTimeout(hoverTimerRef.current);
      hoverTimerRef.current = null;
    }

    const el = videoRef.current;
    if (el) {
      try {
        el.pause();
        el.removeAttribute("src");
        el.load();
      } catch {
        // noop
      }
    }

    setShouldRenderPreview(false);
    setPreviewState("idle");
    setProgress(0);

    if (previewController.getActiveId() === video.id) {
      previewController.setActiveId(null);
    }
  }

  function handleThumbnailError() {
    if (!video.thumbnail.startsWith("/p/thumb/")) return;
    if (thumbnailRetry >= 8 || thumbnailRetryTimerRef.current) return;

    thumbnailRetryTimerRef.current = window.setTimeout(() => {
      thumbnailRetryTimerRef.current = null;
      setThumbnailRetry((n) => n + 1);
    }, Math.min(1000 + thumbnailRetry * 750, 5000));
  }

  const thumbnailSrc =
    thumbnailRetry === 0
      ? video.thumbnail
      : withRetryParam(video.thumbnail, thumbnailRetry);

  function startPreviewIntent() {
    if (!inView) return;
    if (hoverTimerRef.current) return;
    setPreviewState("intent");

    hoverTimerRef.current = window.setTimeout(() => {
      hoverTimerRef.current = null;
      startPreviewNow({ requireInView: true });
    }, HOVER_DELAY_MS);
  }

  function startPreviewNow(options: { requireInView: boolean }) {
    if (options.requireInView && !inView) return;
    if (hoverTimerRef.current) {
      window.clearTimeout(hoverTimerRef.current);
      hoverTimerRef.current = null;
    }
    previewController.setActiveId(video.id);
    setShouldRenderPreview(true);
    setPreviewState("loading");
  }

  function stopPreview() {
    cleanup();
  }

  function handlePointerEnter(event: React.PointerEvent<HTMLElement>) {
    lastPointerTypeRef.current = event.pointerType;
    if (shouldStartInstantPreview({ pointerType: event.pointerType })) return;
    startPreviewIntent();
  }

  function handlePointerLeave(event: React.PointerEvent<HTMLElement>) {
    if (shouldStartInstantPreview({ pointerType: event.pointerType })) return;
    stopPreview();
  }

  function handlePointerDown(event: React.PointerEvent<HTMLElement>) {
    lastPointerTypeRef.current = event.pointerType;
  }

  function handleClickCapture(event: React.MouseEvent<HTMLAnchorElement>) {
    const previewActive = activeId === video.id && shouldRenderPreview;
    if (
      !shouldInterceptPreviewTap({
        pointerType: lastPointerTypeRef.current,
        canHover: canHoverRef.current,
        previewActive,
      })
    ) {
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    startPreviewNow({ requireInView: false });
  }

  return (
    <article
      ref={rootRef as React.RefObject<HTMLElement>}
      className="video-card"
      onPointerEnter={handlePointerEnter}
      onPointerLeave={handlePointerLeave}
      onPointerDown={handlePointerDown}
      onFocus={startPreviewIntent}
      onBlur={stopPreview}
    >
      <Link
        to={video.href}
        className="video-card__link"
        tabIndex={0}
        onClickCapture={handleClickCapture}
      >
        <div className="thumb-frame">
          <img
            className="thumb-image"
            src={thumbnailSrc}
            alt={video.title}
            loading="lazy"
            onError={handleThumbnailError}
          />

          {shouldRenderPreview && (
            <PreviewVideo
              ref={videoRef}
              src={video.previewSrc}
              state={previewState}
              onCanPlay={() => setPreviewState("playing")}
              onError={() => setPreviewState("error")}
              onTimeUpdate={(p) => setProgress(p)}
            />
          )}

          {previewState === "loading" && <span className="preview-loader" />}
          {previewState === "error" && (
            <span className="preview-error">预览加载失败</span>
          )}

          {/* 预览进度条（播放时显示在底部） */}
          {previewState === "playing" && (
            <div className="preview-progress" aria-hidden="true">
              <div
                className="preview-progress__bar"
                style={{ width: `${Math.min(100, progress * 100)}%` }}
              />
            </div>
          )}

          {/* hover 时右上角 "预览" 角标 */}
          {previewState === "playing" && (
            <span className="preview-tag" aria-hidden="true">
              预览
            </span>
          )}

          <div className="badge-row">
            {video.quality === "HD" && (
              <span className="video-badge is-hd">HD</span>
            )}
            {(video.badges ?? []).map((badge) => (
              <span className="video-badge" key={badge}>
                {badge}
              </span>
            ))}
          </div>

          {video.sourceLabel && previewState !== "playing" && (
            <span
              className="source-badge"
              data-kind={sourceKindFromLabel(video.sourceLabel)}
              title={`来源：${video.sourceLabel}`}
            >
              {video.sourceLabel}
            </span>
          )}

          <span className="duration">{video.duration}</span>
        </div>

        <h3 className="video-title" title={video.title}>
          {video.title}
        </h3>

        <div className="video-meta">
          <span className="video-meta__author">{video.author}</span>
          <span>{formatCount(video.views)} 观看</span>
          <span>{video.publishedAt}</span>
        </div>
      </Link>
    </article>
  );
}

function withRetryParam(src: string, retry: number): string {
  const sep = src.includes("?") ? "&" : "?";
  return `${src}${sep}r=${retry}`;
}

// 从后端返回的 sourceLabel 推断网盘类型（用于颜色标识）。
// 后端目前会下发中文名（"夸克网盘" / "115 网盘" / "PikPak" / "联通沃盘" / "OneDrive"）
// 或英文 kind。两边都尝试匹配；都没匹配上时返回空字符串，CSS 会回落到默认色。
function sourceKindFromLabel(label: string): string {
  const value = label.toLowerCase();
  if (value.includes("夸克") || value.includes("quark")) return "quark";
  if (value.includes("115") || value.includes("p115")) return "p115";
  if (value.includes("pikpak")) return "pikpak";
  if (value.includes("沃盘") || value.includes("wopan") || value.includes("联通")) return "wopan";
  if (value.includes("onedrive") || value.includes("one drive")) return "onedrive";
  if (value.includes("本地") || value.includes("localstorage") || value.includes("local storage")) return "localstorage";
  return "";
}
