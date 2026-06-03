import { useId } from "react";
import { kindLabel } from "./constants";
import * as api from "../api";

export function Spider91UploadTargetField({
  value,
  onChange,
  uploadTargets,
}: {
  value: string;
  onChange: (v: string) => void;
  uploadTargets: api.AdminDrive[];
}) {
  const targetId = useId();

  return (
    <div className="admin-form__row">
      <label htmlFor={targetId}>视频上传目标</label>
      <select id={targetId} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">本地保存，不上传</option>
        {uploadTargets.map((d) => (
          <option key={d.id} value={d.id}>
            {kindLabel[d.kind] ?? d.kind} · {d.name || d.id}
          </option>
        ))}
      </select>
      <div className="admin-form__help">
        选择本地保存时，爬取视频只保存在服务器本地；选择 115 网盘、123 云盘、PikPak 或 OneDrive 后，较早的视频会上传到该云盘根目录下的 91 Spider 文件夹。该设置全局生效。
      </div>
    </div>
  );
}
