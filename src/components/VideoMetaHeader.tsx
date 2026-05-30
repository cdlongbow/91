import type { VideoDetail } from "@/types";
import { formatCount } from "@/lib/format";

type Props = {
  video: VideoDetail;
};

/**
 * 详情页标题块。
 *
 * 视觉：
 * - 标题：大、粗、最高两行
 * - meta：作者首字头像 + 名字 + 一组小胶囊（来源、画质、时长、观看数、发布时间）
 *   每个胶囊有自己的语义色彩，避免传统 "·" 分隔列表的列表感。
 */
export function VideoMetaHeader({ video }: Props) {
  const author = (video.author ?? "").trim();
  const source = (video.sourceLabel ?? "").trim();
  const quality = (video.quality ?? "").trim();
  const duration = (video.duration ?? "").trim();
  const published = (video.publishedAt ?? "").trim();
  const sourceKind = sourceKindFromLabel(source);

  return (
    <header className="vd-header">
      <h1 className="vd-header__title" title={video.title}>
        {video.title}
      </h1>

      <div className="vd-header__row">
        {author && (
          <div className="vd-author" aria-label={`作者 ${author}`}>
            <span className="vd-author__avatar" aria-hidden="true">
              {author.slice(0, 1)}
            </span>
            <span className="vd-author__name">{author}</span>
          </div>
        )}

        <ul className="vd-meta" aria-label="视频信息">
          {source && (
            <li className="vd-meta__chip" data-tone={sourceKind || "neutral"}>
              <span className="vd-meta__dot" aria-hidden="true" />
              {source}
            </li>
          )}
          {quality && (
            <li
              className="vd-meta__chip"
              data-tone={quality.toUpperCase() === "HD" ? "accent" : "neutral"}
            >
              {quality}
            </li>
          )}
          {duration && <li className="vd-meta__chip">{duration}</li>}
          <li className="vd-meta__chip">
            <strong>{formatCount(video.views)}</strong> 次观看
          </li>
          {published && <li className="vd-meta__chip">{published}</li>}
        </ul>
      </div>
    </header>
  );
}

// 根据 sourceLabel 识别网盘类型，用于胶囊配色。
function sourceKindFromLabel(label: string): string {
  const value = label.toLowerCase();
  if (value.includes("夸克") || value.includes("quark")) return "quark";
  if (value.includes("115") || value.includes("p115")) return "p115";
  if (value.includes("pikpak")) return "pikpak";
  if (value.includes("沃盘") || value.includes("wopan") || value.includes("联通"))
    return "wopan";
  if (value.includes("onedrive") || value.includes("one drive")) return "onedrive";
  if (value.includes("本地") || value.includes("localstorage") || value.includes("local storage"))
    return "localstorage";
  return "";
}
