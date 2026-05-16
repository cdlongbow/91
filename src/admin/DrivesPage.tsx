import { useEffect, useMemo, useState } from "react";
import { Plus, RefreshCw, RotateCcw, Trash2 } from "lucide-react";
import * as api from "./api";
import { useToast } from "./ToastContext";
import { Modal } from "./Modal";
import { formatBytes } from "./storageFormat";

const kindLabel: Record<string, string> = {
  quark: "夸克网盘",
  p115: "115 网盘",
  pikpak: "PikPak",
  wopan: "联通沃盘",
  onedrive: "OneDrive",
};

type Kind = api.AdminDrive["kind"];

type FormState = {
  id: string;
  kind: Kind;
  name: string;
  rootId: string;
  scanRootId: string;
  creds: Record<string, string>;
};

const emptyForm: FormState = {
  id: "",
  kind: "quark",
  name: "",
  rootId: "0",
  scanRootId: "0",
  creds: {},
};

export function DrivesPage() {
  const [list, setList] = useState<api.AdminDrive[]>([]);
  const [storage, setStorage] = useState<api.AdminDriveStorage | null>(null);
  const [loading, setLoading] = useState(true);
  const [modalOpen, setModalOpen] = useState(false);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [regenFailedId, setRegenFailedId] = useState("");
  const { show } = useToast();

  async function refresh() {
    setLoading(true);
    try {
      const [data, storageData] = await Promise.all([
        api.listDrives(),
        api.getDriveStorage(),
      ]);
      setList(data ?? []);
      setStorage(storageData);
    } catch (e) {
      show(e instanceof Error ? e.message : "加载失败", "error");
    } finally {
      setLoading(false);
    }
  }

  async function refreshDriveList() {
    try {
      const data = await api.listDrives();
      setList(data ?? []);
    } catch {
      // 保持当前页面状态，下一次轮询或手动操作再刷新。
    }
  }

  useEffect(() => {
    refresh();
    const timer = window.setInterval(() => {
      refreshDriveList();
    }, 5000);
    return () => window.clearInterval(timer);
  }, []);

  function openCreate() {
    setForm(emptyForm);
    setModalOpen(true);
  }

  function openEdit(d: api.AdminDrive) {
    setForm({
      id: d.id,
      kind: d.kind,
      name: d.name,
      rootId: d.rootId,
      scanRootId: d.scanRootId || d.rootId,
      creds: {},
    });
    setModalOpen(true);
  }

  async function handleSave() {
    if (!form.id || !form.kind) {
      show("请填 ID 和类型", "error");
      return;
    }
    // 若编辑且没有提供凭证，提示一下但仍允许保存（不改凭证）
    setSaving(true);
    try {
      const resp = await api.upsertDrive({
        id: form.id,
        kind: form.kind,
        name: form.name || form.id,
        rootId: form.rootId || defaultRootId(form.kind),
        scanRootId: form.scanRootId || form.rootId || defaultRootId(form.kind),
        credentials: form.creds,
      });
      if (resp.warning) {
        show(`已保存，但 driver 初始化失败：${resp.warning}`, "error");
      } else {
        show("已保存", "success");
      }
      setModalOpen(false);
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "error");
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete(d: api.AdminDrive) {
    if (!window.confirm(`确定删除 ${d.name || d.id}？\n这会移除盘配置，但不会删除其中的视频元数据。`)) return;
    try {
      await api.deleteDrive(d.id);
      show("已删除", "success");
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "删除失败", "error");
    }
  }

  async function handleRescan(d: api.AdminDrive) {
    try {
      await api.rescan(d.id);
      show("已触发扫描，可稍后刷新视频列表查看", "success");
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    }
  }

  async function handleRegenFailed(d: api.AdminDrive) {
    setRegenFailedId(d.id);
    try {
      await api.regenFailedPreviews(d.id);
      show("已触发失败 teaser 重新生成", "success");
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setRegenFailedId("");
    }
  }

  return (
    <section>
      <header className="admin-page__header">
        <h1 className="admin-page__title">网盘管理</h1>
        <button className="admin-btn is-primary" onClick={openCreate}>
          <Plus size={14} /> 新建
        </button>
      </header>

      {storage && <StorageSummary storage={storage} />}

      {loading ? (
        <div className="admin-empty">加载中...</div>
      ) : list.length === 0 ? (
        <div className="admin-card admin-empty">
          还没有配置任何网盘。点击右上角「新建」，选择夸克 / 115 / PikPak / 沃盘 / OneDrive，填入凭证即可。
        </div>
      ) : (
        <table className="admin-table admin-drives-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>类型</th>
              <th>ID</th>
              <th>状态</th>
              <th>生成状态</th>
              <th>扫描根</th>
              <th>本地占用</th>
              <th>Teaser</th>
              <th className="is-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            {list.map((d) => (
              <tr key={d.id}>
                <td data-label="名称">{d.name || <span style={{ color: "#aaa" }}>（未命名）</span>}</td>
                <td data-label="类型">{kindLabel[d.kind] ?? d.kind}</td>
                <td data-label="ID" style={{ fontFamily: "ui-monospace", fontSize: 12 }}>{d.id}</td>
                <td data-label="状态">
                  <StatusTag status={d.status} error={d.lastError} hasCred={d.hasCredential} />
                </td>
                <td data-label="生成状态">
                  <GenerationStatusCell drive={d} />
                </td>
                <td data-label="扫描根" style={{ fontFamily: "ui-monospace", fontSize: 12 }}>
                  {d.scanRootId || d.rootId}
                </td>
                <td data-label="本地占用">
                  <StorageCell usage={storage?.drives[d.id]} />
                </td>
                <td data-label="Teaser">
                  <TeaserCounts drive={d} />
                </td>
                <td className="is-actions" data-label="操作">
                  <button className="admin-btn" onClick={() => handleRescan(d)}>
                    <RefreshCw size={13} /> 重扫
                  </button>{" "}
                  <button
                    className="admin-btn"
                    disabled={(d.teaserFailedCount ?? 0) <= 0 || regenFailedId === d.id}
                    onClick={() => handleRegenFailed(d)}
                  >
                    <RotateCcw size={13} />
                    <span className="admin-btn__label">
                      {regenFailedId === d.id ? "触发中..." : "重试失败 Teaser"}
                    </span>
                  </button>{" "}
                  <button className="admin-btn" onClick={() => openEdit(d)}>
                    编辑
                  </button>{" "}
                  <button className="admin-btn is-danger" onClick={() => handleDelete(d)}>
                    <Trash2 size={13} />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <Modal
        open={modalOpen}
        title={form.id && list.find((x) => x.id === form.id) ? "编辑网盘" : "新建网盘"}
        onClose={() => setModalOpen(false)}
        footer={
          <>
            <button className="admin-btn" onClick={() => setModalOpen(false)}>
              取消
            </button>
            <button
              className="admin-btn is-primary"
              onClick={handleSave}
              disabled={saving}
            >
              {saving ? "保存中..." : "保存"}
            </button>
          </>
        }
      >
        <DriveForm form={form} onChange={setForm} isEdit={!!list.find((x) => x.id === form.id)} />
      </Modal>
    </section>
  );
}

function StorageSummary({ storage }: { storage: api.AdminDriveStorage }) {
  return (
    <section className="admin-card admin-storage-summary" aria-label="本地媒体存储">
      <div className="admin-storage-summary__metric">
        <span>封面占用</span>
        <strong>{formatBytes(storage.thumbnailBytes)}</strong>
      </div>
      <div className="admin-storage-summary__metric">
        <span>Teaser 占用</span>
        <strong>{formatBytes(storage.teaserBytes)}</strong>
      </div>
      <div className="admin-storage-summary__metric">
        <span>本地媒体合计</span>
        <strong>{formatBytes(storage.totalBytes)}</strong>
      </div>
      <div className="admin-storage-summary__metric">
        <span>磁盘可用</span>
        <strong>{formatBytes(storage.availableBytes)}</strong>
      </div>
    </section>
  );
}

function StorageCell({ usage }: { usage?: api.DriveStorageUsage }) {
  if (!usage || usage.totalBytes <= 0) {
    return <span className="admin-storage-cell__empty">0 B</span>;
  }
  return (
    <div className="admin-storage-cell">
      <strong>{formatBytes(usage.totalBytes)}</strong>
      <span>封面 {formatBytes(usage.thumbnailBytes)}</span>
      <span>Teaser {formatBytes(usage.teaserBytes)}</span>
    </div>
  );
}

function TeaserCounts({ drive }: { drive: api.AdminDrive }) {
  return (
    <div className="admin-teaser-counts">
      <span className="admin-drive-teaser__metric is-ready">
        就绪 {drive.teaserReadyCount ?? 0}
      </span>
      <span className="admin-drive-teaser__metric is-pending">
        待生成 {drive.teaserPendingCount ?? 0}
      </span>
      <span className="admin-drive-teaser__metric is-failed">
        失败 {drive.teaserFailedCount ?? 0}
      </span>
    </div>
  );
}

function GenerationStatusCell({ drive }: { drive: api.AdminDrive }) {
  return (
    <div className="admin-generation-statuses">
      <GenerationStatusLine label="封面" status={drive.thumbnailGenerationStatus} />
      <GenerationStatusLine label="预览" status={drive.previewGenerationStatus} />
    </div>
  );
}

function GenerationStatusLine({
  label,
  status,
}: {
  label: string;
  status?: api.DriveGenerationStatus;
}) {
  const state = status?.state || "idle";
  const queueLength = status?.queueLength ?? 0;
  const detail = generationDetail(status);
  const title = generationTitle(status, detail);
  const countText = queueLength > 0 ? `${label === "封面" ? "剩余" : "队列"} ${queueLength}` : "";

  return (
    <div className="admin-generation-row" title={title}>
      <span className="admin-generation-kind">{label}</span>
      <span className={`admin-status admin-generation-state is-${generationStateClass(state)}`}>
        {generationStateLabel(state)}
      </span>
      {(detail || queueLength > 0) && (
        <span className="admin-generation-detail">
          {[detail, countText].filter(Boolean).join(" / ")}
        </span>
      )}
    </div>
  );
}

function generationStateLabel(state: string): string {
  if (state === "generating") return "生成中";
  if (state === "cooling") return "冷却中";
  if (state === "queued") return "排队中";
  return "空闲";
}

function generationStateClass(state: string): string {
  if (state === "generating" || state === "cooling" || state === "queued") {
    return state;
  }
  return "idle";
}

function generationDetail(status?: api.DriveGenerationStatus): string {
  if (!status) return "";
  if (status.state === "cooling" && status.cooldownUntil) {
    return `剩余 ${formatCooldownRemaining(status.cooldownUntil)}`;
  }
  if (status.currentTitle) {
    return status.currentTitle;
  }
  return "";
}

function generationTitle(status: api.DriveGenerationStatus | undefined, detail: string): string | undefined {
  if (!status) return detail || undefined;
  if (status.state === "cooling" && status.cooldownUntil) {
    return `冷却至 ${formatClock(status.cooldownUntil)}`;
  }
  return status.currentTitle || detail || undefined;
}

function formatCooldownRemaining(value: string): string {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  const totalSeconds = Math.max(0, Math.ceil((d.getTime() - Date.now()) / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) return `${hours}小时${minutes}分`;
  if (minutes > 0) return `${minutes}分${seconds}秒`;
  return `${seconds}秒`;
}

function formatClock(value: string): string {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  return d.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

function StatusTag({
  status,
  error,
  hasCred,
}: {
  status: string;
  error?: string;
  hasCred: boolean;
}) {
  if (!hasCred) {
    return <span className="admin-status is-pending">未配置凭证</span>;
  }
  if (status === "ok") return <span className="admin-status is-ok">已连接</span>;
  if (status === "error")
    return (
      <span className="admin-status is-error" title={error}>
        错误
      </span>
    );
  return <span className="admin-status">{status || "未连接"}</span>;
}

function DriveForm({
  form,
  onChange,
  isEdit,
}: {
  form: FormState;
  onChange: (f: FormState) => void;
  isEdit: boolean;
}) {
  const fields = useMemo(() => credentialFields(form.kind), [form.kind]);

  function set<K extends keyof FormState>(k: K, v: FormState[K]) {
    onChange({ ...form, [k]: v });
  }
  function setCred(k: string, v: string) {
    onChange({ ...form, creds: { ...form.creds, [k]: v } });
  }
  function setKind(v: Kind) {
    onChange({
      ...form,
      kind: v,
      rootId: defaultRootId(v),
      scanRootId: defaultRootId(v),
      creds: {},
    });
  }

  return (
    <div className="admin-form">
      <div className="admin-form__row">
        <label>ID（英文，唯一）</label>
        <input
          value={form.id}
          onChange={(e) => set("id", e.target.value)}
          placeholder="例如 my-quark"
          disabled={isEdit}
        />
        {isEdit && <div className="admin-form__help">已创建的盘 ID 不能修改</div>}
      </div>
      <div className="admin-form__row">
        <label>名称</label>
        <input
          value={form.name}
          onChange={(e) => set("name", e.target.value)}
          placeholder="给这个盘起个名字"
        />
      </div>
      <div className="admin-form__row">
        <label>类型</label>
        <select
          value={form.kind}
          onChange={(e) => setKind(e.target.value as Kind)}
          disabled={isEdit}
        >
          <option value="quark">夸克网盘</option>
          <option value="p115">115 网盘</option>
          <option value="pikpak">PikPak</option>
          <option value="wopan">联通沃盘</option>
          <option value="onedrive">OneDrive</option>
        </select>
      </div>
      <div className="admin-form__row">
        <label>根目录 ID</label>
        <input
          value={form.rootId}
          onChange={(e) => set("rootId", e.target.value)}
          placeholder={form.kind === "pikpak" ? "留空表示根目录" : form.kind === "onedrive" ? "root" : "0"}
        />
      </div>
      <div className="admin-form__row">
        <label>扫描起点目录 ID</label>
        <input
          value={form.scanRootId}
          onChange={(e) => set("scanRootId", e.target.value)}
          placeholder="留空则使用根目录"
        />
        <div className="admin-form__help">
          可以指定一个子目录作为视频库入口，避免扫描整个网盘
        </div>
      </div>

      <hr style={{ border: 0, borderTop: "1px solid #eee", margin: "8px 0" }} />

      <div className="admin-form__help" style={{ fontSize: 13, color: "#555" }}>
        {credentialHelp(form.kind, isEdit)}
      </div>

      {fields.map((f) => (
        <div key={f.key} className="admin-form__row">
          <label>{f.label}{f.required && " *"}</label>
          {f.multiline ? (
            <textarea
              value={form.creds[f.key] ?? ""}
              onChange={(e) => setCred(f.key, e.target.value)}
              placeholder={f.placeholder}
            />
          ) : (
            <input
              value={form.creds[f.key] ?? ""}
              onChange={(e) => setCred(f.key, e.target.value)}
              placeholder={f.placeholder}
            />
          )}
          {f.help && <div className="admin-form__help">{f.help}</div>}
        </div>
      ))}
    </div>
  );
}

function credentialHelp(kind: Kind, isEdit: boolean): string {
  const note = isEdit ? "如不修改凭证，留空即可，保存时会沿用旧值。" : "";
  switch (kind) {
    case "quark":
      return `在 pan.quark.cn 登录后，F12 → Network → 任意请求 → Request Headers 里复制整段 Cookie 粘贴到下方。${note}`;
    case "p115":
      return `登录 115.com 后复制 Cookie，形如 "UID=...; CID=...; SEID=...; KID=..."。${note}`;
    case "pikpak":
      return `参考 OpenList 的 PikPak 登录方式。可填用户名和密码首次登录，也可填 refresh_token；如返回验证码链接，打开验证后把 captcha_token 粘贴回来。${note}`;
    case "wopan":
      return `需要 access_token 和 refresh_token。后续会加扫码/短信登录入口，第一版只能手工粘贴。${note}`;
    case "onedrive":
      return `按 OpenList 默认方式，通过 api.oplist.org 在线刷新 token。只需要 refresh_token；保存后会自动回写新的 access_token / refresh_token。${note}`;
    default:
      return "";
  }
}

function credentialFields(kind: Kind): Array<{
  key: string;
  label: string;
  placeholder: string;
  multiline?: boolean;
  required?: boolean;
  help?: string;
}> {
  switch (kind) {
    case "quark":
      return [
        {
          key: "cookie",
          label: "Cookie",
          placeholder: "__pus=...; __puus=...; ...",
          multiline: true,
          required: true,
        },
      ];
    case "p115":
      return [
        {
          key: "cookie",
          label: "Cookie",
          placeholder: "UID=xxx; CID=xxx; SEID=xxx; KID=xxx",
          multiline: true,
          required: true,
        },
      ];
    case "pikpak":
      return [
        {
          key: "username",
          label: "用户名 / 邮箱（无 refresh_token 时必填）",
          placeholder: "user@example.com",
        },
        {
          key: "password",
          label: "密码（无 refresh_token 时必填）",
          placeholder: "PikPak 密码",
        },
        {
          key: "platform",
          label: "platform",
          placeholder: "web（可选：android / web / pc）",
          help: "默认 web；如果登录或直链异常，可尝试 android 或 pc。",
        },
        {
          key: "refresh_token",
          label: "refresh_token（可选）",
          placeholder: "已有 token 时可直接粘贴",
          multiline: true,
        },
        {
          key: "captcha_token",
          label: "captcha_token（可选）",
          placeholder: "遇到验证码校验时粘贴",
          multiline: true,
        },
        {
          key: "device_id",
          label: "device_id（可选）",
          placeholder: "留空自动生成并保存",
        },
        {
          key: "disable_media_link",
          label: "disable_media_link",
          placeholder: "true",
          help: "默认 true，使用原始下载链接；填 false 可尝试使用媒体缓存链接。",
        },
      ];
    case "wopan":
      return [
        {
          key: "access_token",
          label: "access_token",
          placeholder: "",
          required: true,
        },
        {
          key: "refresh_token",
          label: "refresh_token",
          placeholder: "",
          required: true,
        },
        {
          key: "family_id",
          label: "family_id（家庭空间可选）",
          placeholder: "留空走个人空间",
        },
      ];
    case "onedrive":
      return [
        {
          key: "refresh_token",
          label: "refresh_token",
          placeholder: "OpenList OneDrive refresh_token",
          multiline: true,
          required: true,
        },
        {
          key: "access_token",
          label: "access_token（可选）",
          placeholder: "留空也可以，保存时会在线刷新",
          multiline: true,
        },
        {
          key: "api_url_address",
          label: "api_url_address（可选）",
          placeholder: "https://api.oplist.org/onedrive/renewapi",
          help: "默认使用 OpenList 的在线刷新 API；除非你有自建兼容服务，否则留空。",
        },
        {
          key: "region",
          label: "region（可选）",
          placeholder: "global（可选：global / cn / us / de）",
          help: "默认 global；世纪互联填 cn，美国政府云填 us，德国云填 de。",
        },
        {
          key: "is_sharepoint",
          label: "is_sharepoint（可选）",
          placeholder: "false",
          help: "普通 OneDrive 留空或 false；SharePoint 文档库填 true，并填写 site_id。",
        },
        {
          key: "site_id",
          label: "site_id（SharePoint 必填）",
          placeholder: "SharePoint site id",
        },
      ];
  }
}

function defaultRootId(kind: Kind): string {
  if (kind === "pikpak") return "";
  if (kind === "onedrive") return "root";
  return "0";
}
