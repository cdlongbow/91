import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ArrowLeft,
  ChevronDown,
  ChevronRight,
  Download,
  FolderTree,
  HardDrive,
  PlayCircle,
  Plus,
  Power,
  PowerOff,
  RefreshCw,
  RotateCcw,
  Trash2,
} from "lucide-react";
import * as api from "./api";
import { useToast } from "./ToastContext";
import { Modal } from "./Modal";
import { formatBytes } from "./storageFormat";
import { makeUniqueDriveId } from "./driveId";

const kindLabel: Record<string, string> = {
  quark: "夸克网盘",
  p115: "115 网盘",
  pikpak: "PikPak",
  wopan: "联通沃盘",
  onedrive: "OneDrive",
  spider91: "91 爬虫",
};

type Kind = api.AdminDrive["kind"];

type FormState = {
  /**
   * 内部稳定标识。编辑现有网盘时由后端数据填入；新建时不展示给用户，
   * 保存前根据名称和类型自动生成。
   */
  id: string;
  kind: Kind;
  name: string;
  rootId: string;
  scanRootId: string;
  creds: Record<string, string>;
  /**
   * spider91 专用字段：把视频迁移到云盘的目标 drive ID。
   * 实际值不会和 creds 一起 POST 到 /admin/api/drives，而是在 handleSave 里
   * 单独通过 PUT /admin/api/settings 写到全局 setting。在 form state 里维护它
   * 是为了让 DriveForm 能读写同一份编辑状态。
   *
   * 空字符串 = 本地保存，不上传。
   */
  spider91UploadDriveId: string;
};

const emptyForm: FormState = {
  id: "",
  kind: "p115",
  name: "",
  rootId: "0",
  scanRootId: "0",
  creds: {},
  spider91UploadDriveId: "",
};

export function DrivesPage() {
  const [list, setList] = useState<api.AdminDrive[]>([]);
  const [storage, setStorage] = useState<api.AdminDriveStorage | null>(null);
  const [settings, setSettings] = useState<api.Settings | null>(null);
  const [loading, setLoading] = useState(true);
  const [modalOpen, setModalOpen] = useState(false);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [regenFailedId, setRegenFailedId] = useState("");
  // 与 regenFailedId 并列：失败封面重新入队按钮的 disable 状态。两套独立按钮 →
  // 两个 state 互不阻塞，避免操作 teaser 时锁住封面那条按钮（反之亦然）。
  const [regenFailedThumbId, setRegenFailedThumbId] = useState("");
  // togglingTeaserId 在请求未返回前禁用按钮，避免连点导致两次切换互相覆盖。
  const [togglingTeaserId, setTogglingTeaserId] = useState("");
  const [selectedDriveId, setSelectedDriveId] = useState<string | null>(null);
  const { show } = useToast();

  // 当前系统中可作为 spider91 上传目标的 drive 列表（pikpak ∪ p115）。
  // 用户保存 spider91 drive 时从这里挑一个；空表示本地保存不上传。
  const uploadTargets = useMemo(
    () => list.filter((d) => d.kind === "pikpak" || d.kind === "p115"),
    [list]
  );

  async function refresh() {
    setLoading(true);
    try {
      const [data, storageData, settingsData] = await Promise.all([
        api.listDrives(),
        api.getDriveStorage(),
        api.getSettings().catch(() => null),
      ]);
      setList(data ?? []);
      setStorage(storageData);
      if (settingsData) setSettings(settingsData);
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
    // 创建时把全局 setting 当前值带进表单，方便用户在新建第一个 spider91 drive 时
    // 直接看到当前的上传目标选择（一般是空 = 本地保存）。
    setForm({
      ...emptyForm,
      spider91UploadDriveId: settings?.spider91UploadDriveId ?? "",
    });
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
      spider91UploadDriveId: settings?.spider91UploadDriveId ?? "",
    });
    setModalOpen(true);
  }

  async function handleSave() {
    const name = form.name.trim();
    if (!name || !form.kind) {
      show("请填名称和类型", "error");
      return;
    }
    const existing = list.find((x) => x.id === form.id);
    const driveID = existing
      ? form.id
      : makeUniqueDriveId(form.kind, name, list);
    // 若编辑且没有提供凭证，提示一下但仍允许保存（不改凭证）
    setSaving(true);
    try {
      const resp = await api.upsertDrive({
        id: driveID,
        kind: form.kind,
        name,
        rootId: form.rootId || defaultRootId(form.kind),
        scanRootId: form.scanRootId || form.rootId || defaultRootId(form.kind),
        credentials: form.creds,
      });

      // 仅当编辑/新建的是 spider91 drive 时，才同步全局上传目标 setting。
      // 避免动其它类型 drive 的表单顺手覆盖了这个独立设置。
      if (form.kind === "spider91" && form.spider91UploadDriveId !== (settings?.spider91UploadDriveId ?? "")) {
        try {
          const updated = await api.updateSettings({
            spider91UploadDriveId: form.spider91UploadDriveId,
          });
          setSettings(updated);
        } catch (settingsErr) {
          // 不阻断主流程：drive 已经存了，setting 没存上，由 toast 提示用户手动重试
          show(
            settingsErr instanceof Error
              ? `Drive 已保存，但上传目标设置失败：${settingsErr.message}`
              : "上传目标设置失败",
            "error"
          );
          setModalOpen(false);
          refresh();
          return;
        }
      }

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
      if (d.kind === "spider91") {
        show("已触发抓取任务，需要 2-4 分钟，可稍后刷新视频列表查看", "success");
      } else {
        show("已触发扫描，可稍后刷新视频列表查看", "success");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    }
  }

  /**
   * 立即触发完整凌晨流水线（Phase1 扫所有云盘 → Phase2 spider91 爬虫 →
   * Phase3 spider91 → 云盘迁移）。后端立即返回 202；进度看 backend 日志。
   * 如果当前已有流水线在跑，后端最多保留一个待触发请求，当前轮结束后再跑一轮。
   */
  async function handleRunNightly() {
    try {
      await api.runNightlyJob();
      show("已触发扫描所有网盘，耗时较长，可在 backend 日志观察进度", "success");
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

  // 失败封面图重生：与 handleRegenFailed 对称（一个管 teaser，一个管封面）。
  async function handleRegenFailedThumbnails(d: api.AdminDrive) {
    setRegenFailedThumbId(d.id);
    try {
      await api.regenFailedThumbnails(d.id);
      show("已触发失败封面重新生成", "success");
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setRegenFailedThumbId("");
    }
  }

  async function handleToggleTeaser(d: api.AdminDrive) {
    const next = !d.teaserEnabled;
    setTogglingTeaserId(d.id);
    // 乐观更新本地状态，操作流畅；失败再回滚。
    setList((prev) =>
      prev.map((item) =>
        item.id === d.id ? { ...item, teaserEnabled: next } : item
      )
    );
    try {
      const resp = await api.setDriveTeaserEnabled(d.id, next);
      show(
        resp.teaserEnabled
          ? `已开启「${d.name || d.id}」的 Teaser 生成`
          : `已关闭「${d.name || d.id}」的 Teaser 生成`,
        "success"
      );
      // 以服务端响应为准（防止极端竞态）；并刷新计数等附属数据。
      setList((prev) =>
        prev.map((item) =>
          item.id === d.id ? { ...item, teaserEnabled: resp.teaserEnabled } : item
        )
      );
      refreshDriveList();
    } catch (e) {
      // 回滚乐观更新
      setList((prev) =>
        prev.map((item) =>
          item.id === d.id ? { ...item, teaserEnabled: d.teaserEnabled } : item
        )
      );
      show(e instanceof Error ? e.message : "切换失败", "error");
    } finally {
      setTogglingTeaserId("");
    }
  }

  const selectedDrive = useMemo(() => {
    return selectedDriveId ? list.find((d) => d.id === selectedDriveId) : null;
  }, [selectedDriveId, list]);

  if (selectedDriveId && selectedDrive) {
    const d = selectedDrive;
    const driveStorage = storage?.drives[d.id];

    return (
      <section>
        <header className="admin-drive-detail__header-bar">
          <button
            type="button"
            className="admin-drive-detail__back-btn"
            onClick={() => setSelectedDriveId(null)}
            title="返回网盘列表"
          >
            <ArrowLeft size={16} />
          </button>
          <div className="admin-drive-detail__title-wrap">
            <h1 className="admin-drive-detail__title">{d.name || d.id}</h1>
            <span className="admin-mono-cell" style={{ fontSize: "14px", color: "var(--text-faint)" }}>
              ({d.id})
            </span>
          </div>
        </header>

        <div className="admin-drive-detail-layout">
          {/* 左栏：基本状态与控制 */}
          <div>
            <div className="admin-detail-card">
              <header className="admin-detail-card__title">
                <div className="admin-detail-card__title-left">
                  <HardDrive size={16} />
                  <span>基本信息与状态</span>
                </div>
                <StatusTag kind={d.kind} status={d.status} error={d.lastError} hasCred={d.hasCredential} />
              </header>

              <div className="admin-detail-grid">
                <div className="admin-detail-row">
                  <span className="admin-detail-label">网盘类型</span>
                  <span className="admin-detail-value">{kindLabel[d.kind] ?? d.kind}</span>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">网盘 ID</span>
                  <span className="admin-detail-value admin-mono-cell">{d.id}</span>
                </div>
                {d.kind !== "spider91" && (
                  <>
                    <div className="admin-detail-row">
                      <span className="admin-detail-label">根目录 ID</span>
                      <span className="admin-detail-value admin-mono-cell">{d.rootId}</span>
                    </div>
                    <div className="admin-detail-row">
                      <span className="admin-detail-label">扫描起点 ID</span>
                      <span className="admin-detail-value admin-mono-cell">{d.scanRootId || d.rootId}</span>
                    </div>
                  </>
                )}
                {d.kind === "spider91" && (
                  <div className="admin-detail-row">
                    <span className="admin-detail-label">上次抓取时间</span>
                    <span className="admin-detail-value">
                      {d.lastCrawlAt ? new Date(d.lastCrawlAt * 1000).toLocaleString() : "尚未抓取"}
                    </span>
                  </div>
                )}
                {d.lastError && (
                  <div className="admin-detail-row" style={{ alignItems: "start" }}>
                    <span className="admin-detail-label">最后一次错误</span>
                    <span className="admin-detail-value" style={{ color: "var(--danger)" }}>
                      {d.lastError}
                    </span>
                  </div>
                )}
              </div>

              <div className="admin-detail-actions">
                <button className="admin-btn is-primary" onClick={() => handleRescan(d)}>
                  {d.kind === "spider91" ? (
                    <>
                      <Download size={13} /> 立即抓取
                    </>
                  ) : (
                    <>
                      <RefreshCw size={13} /> 立即重扫
                    </>
                  )}
                </button>
                <button className="admin-btn" onClick={() => openEdit(d)}>
                  {d.kind === "spider91" ? "编辑配置" : "编辑配置凭证"}
                </button>
                <button className="admin-btn is-danger" onClick={() => {
                  handleDelete(d);
                  setSelectedDriveId(null);
                }} style={{ marginLeft: "auto" }}>
                  <Trash2 size={13} /> 删除网盘
                </button>
              </div>
            </div>

            {/* 如果不是爬虫网盘，内嵌显示跳过目录设置 */}
            {d.kind !== "spider91" && (
              <SkipDirsPanel
                drive={d}
                onSaved={(saved) => {
                  setList((prev) =>
                    prev.map((item) =>
                      item.id === saved.id
                        ? { ...item, skipDirIds: saved.skipDirIds }
                        : item
                    )
                  );
                  refreshDriveList();
                }}
              />
            )}
          </div>

          {/* 右栏：Teaser / 封面 / 指纹 与 缓存占用 */}
          <div>
            <div className="admin-detail-card">
              <header className="admin-detail-card__title">
                <div className="admin-detail-card__title-left">
                  <PlayCircle size={16} />
                  <span>生成状态</span>
                </div>
                <div className="admin-detail-actions-inline">
                  <button
                    className={`admin-btn ${d.teaserEnabled ? "is-success" : ""}`}
                    onClick={() => handleToggleTeaser(d)}
                    disabled={togglingTeaserId === d.id}
                    style={{ padding: "4px 10px", fontSize: "11px" }}
                  >
                    {d.teaserEnabled ? <Power size={11} /> : <PowerOff size={11} />}
                    <span>{d.teaserEnabled ? "Teaser: 开" : "Teaser: 关"}</span>
                  </button>
                </div>
              </header>

              <div className="admin-detail-grid">
                <div className="admin-detail-row">
                  <span className="admin-detail-label">封面状态</span>
                  <div className="admin-detail-value">
                    <GenerationStatusLine label="封面" status={d.thumbnailGenerationStatus} />
                  </div>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">封面数量</span>
                  <div className="admin-detail-value">
                    <GenerationCounts
                      ready={d.thumbnailReadyCount}
                      pending={d.thumbnailPendingCount}
                      failed={d.thumbnailFailedCount}
                    />
                  </div>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">Teaser 状态</span>
                  <div className="admin-detail-value">
                    <GenerationStatusLine label="预览" status={d.previewGenerationStatus} />
                  </div>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">Teaser 数量</span>
                  <div className="admin-detail-value">
                    <GenerationCounts
                      ready={d.teaserReadyCount}
                      pending={d.teaserPendingCount}
                      failed={d.teaserFailedCount}
                    />
                  </div>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">指纹状态</span>
                  <div className="admin-detail-value">
                    <GenerationStatusLine label="指纹" status={d.fingerprintGenerationStatus} />
                  </div>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">指纹数量</span>
                  <div className="admin-detail-value">
                    <GenerationCounts
                      ready={d.fingerprintReadyCount}
                      pending={d.fingerprintPendingCount}
                      failed={d.fingerprintFailedCount}
                    />
                  </div>
                </div>
              </div>

              <div className="admin-detail-actions">
                <button
                  className="admin-btn"
                  disabled={(d.teaserFailedCount ?? 0) <= 0 || regenFailedId === d.id}
                  onClick={() => handleRegenFailed(d)}
                >
                  <RotateCcw size={13} />
                  <span>重试失败 Teaser</span>
                </button>
                <button
                  className="admin-btn"
                  disabled={(d.thumbnailFailedCount ?? 0) <= 0 || regenFailedThumbId === d.id}
                  onClick={() => handleRegenFailedThumbnails(d)}
                >
                  <RotateCcw size={13} />
                  <span>重试失败封面</span>
                </button>
              </div>
            </div>

            <div className="admin-detail-card">
              <header className="admin-detail-card__title">
                <div className="admin-detail-card__title-left">
                  <FolderTree size={16} />
                  <span>本地存储占用</span>
                </div>
              </header>

              <div className="admin-detail-grid">
                <div className="admin-detail-row">
                  <span className="admin-detail-label">封面占用</span>
                  <span className="admin-detail-value">{formatBytes(driveStorage?.thumbnailBytes ?? 0)}</span>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">Teaser 占用</span>
                  <span className="admin-detail-value">{formatBytes(driveStorage?.teaserBytes ?? 0)}</span>
                </div>
                <div className="admin-detail-row">
                  <span className="admin-detail-label">本地总占用</span>
                  <span className="admin-detail-value" style={{ fontWeight: "bold" }}>
                    {formatBytes(driveStorage?.totalBytes ?? 0)}
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>

        <Modal
          open={modalOpen}
          title="编辑网盘"
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
          <DriveForm
            form={form}
            onChange={setForm}
            isEdit={true}
            uploadTargets={uploadTargets}
          />
        </Modal>
      </section>
    );
  }

  return (
    <section>
      <header className="admin-page__header">
        <h1 className="admin-page__title">网盘管理</h1>
        <div style={{ display: "flex", gap: "8px" }}>
          <button
            type="button"
            className="admin-btn"
            onClick={handleRunNightly}
            title="立即扫描所有网盘。耗时较长，期间不要重复触发。"
          >
            <PlayCircle size={14} /> 扫描所有网盘
          </button>
          <button className="admin-btn is-primary" onClick={openCreate}>
            <Plus size={14} /> 新建网盘
          </button>
        </div>
      </header>

      {storage && <StorageSummary storage={storage} />}

      {loading ? (
        <div className="admin-empty">加载中...</div>
      ) : list.length === 0 ? (
        <div className="admin-card admin-empty">
          还没有配置任何网盘。点击右上角「新建」，选择夸克 / 115 / PikPak / 沃盘 / OneDrive，填入凭证即可。
        </div>
      ) : (
        <div className="admin-drives-grid">
          {list.map((d) => (
            <div
              key={d.id}
              className="admin-drive-card"
              onClick={() => setSelectedDriveId(d.id)}
            >
              <div className="admin-drive-card__header">
                <div className="admin-drive-card__title">
                  <span className="admin-drive-card__brand-icon" data-kind={d.kind}>
                    {d.kind.substring(0, 2)}
                  </span>
                  <span>{d.name || d.id}</span>
                </div>
                <StatusTag kind={d.kind} status={d.status} error={d.lastError} hasCred={d.hasCredential} />
              </div>

              <div className="admin-drive-card__info">
                <div className="admin-drive-card__metric">
                  <span>封面数 (就绪/失败)</span>
                  <strong>
                    {d.thumbnailReadyCount ?? 0}
                    <span style={{ fontSize: "11px", fontWeight: "normal", color: "var(--text-faint)" }}>
                      {" "}/ {d.thumbnailFailedCount ?? 0}
                    </span>
                  </strong>
                </div>
                <div className="admin-drive-card__metric">
                  <span>Teaser 数 (就绪/失败)</span>
                  <strong>
                    {d.teaserReadyCount ?? 0}
                    <span style={{ fontSize: "11px", fontWeight: "normal", color: "var(--text-faint)" }}>
                      {" "}/ {d.teaserFailedCount ?? 0}
                    </span>
                  </strong>
                </div>
                <div className="admin-drive-card__metric">
                  <span>指纹数 (就绪/失败)</span>
                  <strong>
                    {d.fingerprintReadyCount ?? 0}
                    <span style={{ fontSize: "11px", fontWeight: "normal", color: "var(--text-faint)" }}>
                      {" "}/ {d.fingerprintFailedCount ?? 0}
                    </span>
                  </strong>
                </div>
              </div>

              <div className="admin-drive-card__footer">
                <span>本地占用: {formatBytes(storage?.drives[d.id]?.totalBytes ?? 0)}</span>
                <span className="admin-drive-card__manage-link">
                  管理 <ChevronRight size={14} />
                </span>
              </div>
            </div>
          ))}
        </div>
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
        <DriveForm
          form={form}
          onChange={setForm}
          isEdit={!!list.find((x) => x.id === form.id)}
          uploadTargets={uploadTargets}
        />
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

function GenerationCounts({
  ready,
  pending,
  failed,
}: {
  ready?: number;
  pending?: number;
  failed?: number;
}) {
  return (
    <div className="admin-generation-counts">
      <span className="admin-drive-teaser__metric is-ready">
        就绪 {ready ?? 0}
      </span>
      <span className="admin-drive-teaser__metric is-pending">
        待生成 {pending ?? 0}
      </span>
      <span className="admin-drive-teaser__metric is-failed">
        失败 {failed ?? 0}
      </span>
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
  kind,
  status,
  error,
  hasCred,
}: {
  kind: string;
  status: string;
  error?: string;
  hasCred: boolean;
}) {
  // spider91 没有用户凭证概念，直接看 status；保存后默认就是 "ok"
  if (kind !== "spider91" && !hasCred) {
    return <span className="admin-status is-pending">未配置凭证</span>;
  }
  if (status === "ok") {
    if (kind === "spider91") {
      return <span className="admin-status is-ok">已就绪</span>;
    }
    return <span className="admin-status is-ok">已连接</span>;
  }
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
  uploadTargets,
}: {
  form: FormState;
  onChange: (f: FormState) => void;
  isEdit: boolean;
  uploadTargets: api.AdminDrive[];
}) {
  const fields = useMemo(() => credentialFields(form.kind), [form.kind]);
  const help = credentialHelp(form.kind, isEdit);
  const showDirectoryFields = form.kind !== "spider91" && form.kind !== "onedrive";

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
        <label>名称 *</label>
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
          <option value="p115">115 网盘</option>
          <option value="pikpak">PikPak</option>
          <option value="spider91">91 爬虫</option>
          <option value="quark">夸克网盘</option>
          <option value="wopan">联通沃盘</option>
          <option value="onedrive">OneDrive</option>
        </select>
      </div>
      {showDirectoryFields && (
        <>
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
        </>
      )}

      {(help || fields.length > 0) && (
        <>
          <hr className="admin-form__divider" />

          {help && (
            <div className="admin-form__help admin-form__help--lead">
              {help}
            </div>
          )}

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
        </>
      )}

      {form.kind === "spider91" && (
        <>
          <hr className="admin-form__divider" />
          <Spider91UploadTargetField
            value={form.spider91UploadDriveId}
            onChange={(v) => set("spider91UploadDriveId", v)}
            uploadTargets={uploadTargets}
          />
        </>
      )}
    </div>
  );
}

/**
 * Spider91UploadTargetField 是 spider91 drive 表单专属的"上传目标"下拉。
 *
 * 行为：
 *   - 选项 = "本地保存，不上传" + 系统中所有 pikpak/p115 drive
 *   - value="" 时后端不迁移上传，视频保存在服务器本地
 *   - 没有任何 pikpak/p115 drive 时仍允许选择本地保存
 *   - 该字段写入的是全局 setting `spider91.upload_drive_id`，不是 drive 自己的
 *     credentials —— 所有 spider91 drive 共享同一个上传目标
 */
function Spider91UploadTargetField({
  value,
  onChange,
  uploadTargets,
}: {
  value: string;
  onChange: (v: string) => void;
  uploadTargets: api.AdminDrive[];
}) {
  return (
    <div className="admin-form__row">
      <label>视频上传目标</label>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">本地保存，不上传</option>
        {uploadTargets.map((d) => (
          <option key={d.id} value={d.id}>
            {kindLabel[d.kind] ?? d.kind} · {d.name || d.id}
          </option>
        ))}
      </select>
      <div className="admin-form__help">
        选择本地保存时，爬取视频只保存在服务器本地；选择 115 网盘或 PikPak 后，较早的视频会上传到该云盘根目录。该设置全局生效。
      </div>
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
      return `按 OpenList 默认应用在线挂载，只需要 refresh_token；保存时会自动刷新并保存 token。${note}`;
    case "spider91":
      return "91 爬虫会把定时抓取到的视频和封面先保存到本机，并作为一个视频来源接入站点；它不是外部网盘，不需要填写 Cookie 或目录 ID。后续流水线会把较早的视频上传到你选择的 115 / PikPak 目标盘。";
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
      ];
    case "spider91":
      return [];
  }
}

function defaultRootId(kind: Kind): string {
  if (kind === "pikpak") return "";
  if (kind === "onedrive") return "root";
  if (kind === "spider91") return "/";
  return "0";
}


// ---------- SkipDirsModal ----------
//
// "设置跳过目录"弹窗：
// - 顶部说明 + 已选 chips（点 × 移除）
// - 树形浏览器，按需展开（每展开一层调一次 listDriveDirChildren），勾选目录加入跳过集合
// - 底部"保存"调 setDriveSkipDirIds 整体覆盖；"取消"丢弃改动
//
// 设计取舍：
// - 不一次性递归整棵树。115 等慢盘列目录有限频，按需展开体验稳定也避免风控
// - 选中的目录 ID 直接对应 catalog.drives.skip_dir_ids；不存路径，因为同名目录
//   在不同父级下可能各自需要决定是否跳过，ID 是网盘侧的稳定句柄
// - 已选集合显示在顶部 chips；树里被选中的目录用样式标出，用户在浏览中可以一眼
//   看到自己选了什么
// - 子目录的勾选不影响父目录的跳过判定（scanner 只按 ID 比对），但展示上加视觉
//   线索：父目录被跳过 → 整个子树灰显（提示用户"已被祖先跳过"），仍可单独勾选
// ---------- SkipDirsPanel ----------
//
// "设置跳过目录"面板：
// - 勾选目录加入跳过集合
// - "保存更改"调 setDriveSkipDirIds 整体覆盖
type SkipDirsPanelProps = {
  drive: api.AdminDrive;
  onSaved: (saved: { id: string; skipDirIds: string[] }) => void;
};

function SkipDirsPanel({ drive, onSaved }: SkipDirsPanelProps) {
  const { show } = useToast();
  // selected 用 Set 方便 O(1) 增删 / contains 查询。
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(drive.skipDirIds ?? [])
  );
  const [saving, setSaving] = useState(false);

  // 当外部的 drive 对象改变时，重置内部选中状态，确保在切换详情页时，数据能正确同步
  useEffect(() => {
    setSelected(new Set(drive.skipDirIds ?? []));
  }, [drive.id, drive.skipDirIds]);

  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }, []);

  async function handleSave() {
    setSaving(true);
    try {
      const ids = Array.from(selected);
      const resp = await api.setDriveSkipDirIds(drive.id, ids);
      onSaved({ id: drive.id, skipDirIds: resp.skipDirIds });
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "error");
    } finally {
      setSaving(false);
    }
  }

  const selectedList = useMemo(() => Array.from(selected), [selected]);

  return (
    <div className="admin-detail-card">
      <header className="admin-detail-card__title">
        <div className="admin-detail-card__title-left">
          <FolderTree size={16} />
          <span>扫描跳过目录</span>
        </div>
        <button
          className="admin-btn is-primary"
          onClick={handleSave}
          disabled={saving}
          style={{ padding: "4px 10px", fontSize: "12px", height: "auto" }}
        >
          {saving ? "保存中..." : `保存更改 (${selectedList.length})`}
        </button>
      </header>

      <div style={{ display: "flex", flexDirection: "column", gap: "12px" }}>
        <p className="admin-text-faint" style={{ margin: 0, fontSize: "12px", lineHeight: "1.5" }}>
          勾选要在扫描时跳过的目录。命中目录及其全部子目录都不会被递归扫描。下次扫描生效。
        </p>

        <SelectedDirsChips
          drive={drive}
          selected={selectedList}
          onRemove={toggle}
        />

        <div className="admin-detail-tree-container">
          <DirTreeNode
            driveId={drive.id}
            id="" // 空 = 让后端用 RootID
            name={drive.name || drive.id}
            depth={0}
            initiallyOpen
            ancestorSkipped={false}
            selected={selected}
            onToggle={toggle}
          />
        </div>
      </div>
    </div>
  );
}

// SelectedDirsChips 显示已选目录的 ID 列表（chips）；目录"名"无法在不展开树
// 的情况下拿到（树是按需展开的），所以这里只显示 ID + drive 信息。点 × 移除。
function SelectedDirsChips({
  drive,
  selected,
  onRemove,
}: {
  drive: api.AdminDrive;
  selected: string[];
  onRemove: (id: string) => void;
}) {
  if (selected.length === 0) {
    return (
      <div
        className="admin-text-faint"
        style={{ fontSize: "13px", padding: "6px 0" }}
      >
        当前未勾选任何跳过目录（{drive.kind === "p115" ? "115 网盘" : drive.kind}{" "}
        将完整扫描）。
      </div>
    );
  }
  return (
    <div style={{ display: "flex", flexWrap: "wrap", gap: "6px" }}>
      {selected.map((id) => (
        <span
          key={id}
          className="admin-mono-cell"
          style={{
            display: "inline-flex",
            alignItems: "center",
            gap: "6px",
            padding: "3px 10px",
            border: "1px solid var(--border-subtle)",
            borderRadius: "999px",
            fontSize: "12px",
          }}
          title="点击 × 移除"
        >
          {id}
          <button
            type="button"
            onClick={() => onRemove(id)}
            style={{
              border: "none",
              background: "transparent",
              cursor: "pointer",
              color: "var(--text-secondary)",
              padding: 0,
              lineHeight: 1,
              fontSize: "14px",
            }}
            aria-label={`移除 ${id}`}
          >
            ×
          </button>
        </span>
      ))}
    </div>
  );
}

// DirTreeNode：树的一个节点；按需展开（onClick 触发 listDriveDirChildren）。
//
// - id="" 时表示根节点，调用 dirtree 时不传 parent → 后端用 drive 的 RootID
// - depth=0 不展示 chevron 切换（根总是展开）
// - ancestorSkipped=true 表示某个祖先已被勾选跳过 → 子树灰显但仍允许操作
//   （考虑到用户可能想取消祖先转而精细勾选，UI 上不强制禁用）
type DirTreeNodeProps = {
  driveId: string;
  id: string;
  name: string;
  depth: number;
  initiallyOpen?: boolean;
  ancestorSkipped: boolean;
  selected: Set<string>;
  onToggle: (id: string) => void;
};

function DirTreeNode({
  driveId,
  id,
  name,
  depth,
  initiallyOpen,
  ancestorSkipped,
  selected,
  onToggle,
}: DirTreeNodeProps) {
  const [open, setOpen] = useState(!!initiallyOpen);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [children, setChildren] = useState<api.DriveDirEntry[]>([]);
  const [error, setError] = useState("");

  const isRoot = depth === 0;
  const isSelected = id !== "" && selected.has(id);
  const dimmed = ancestorSkipped;

  const loadChildren = useCallback(async () => {
    if (loaded || loading) return;
    setLoading(true);
    setError("");
    try {
      const data = await api.listDriveDirChildren(driveId, id || undefined);
      setChildren(data ?? []);
      setLoaded(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, [driveId, id, loaded, loading]);

  useEffect(() => {
    if (open && !loaded) {
      void loadChildren();
    }
  }, [open, loaded, loadChildren]);

  function handleToggleOpen() {
    setOpen((v) => !v);
  }

  return (
    <div
      style={{
        paddingLeft: depth === 0 ? 0 : 16,
        opacity: dimmed && !isSelected ? 0.55 : 1,
      }}
    >
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: "6px",
          padding: "4px 6px",
          borderRadius: "4px",
          background: isSelected ? "var(--accent-soft, rgba(255,140,0,0.12))" : "transparent",
        }}
      >
        {!isRoot ? (
          <button
            type="button"
            onClick={handleToggleOpen}
            style={{
              border: "none",
              background: "transparent",
              cursor: "pointer",
              padding: 0,
              display: "inline-flex",
              alignItems: "center",
            }}
            aria-label={open ? "折叠" : "展开"}
          >
            {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          </button>
        ) : (
          <span style={{ width: 14, display: "inline-block" }} />
        )}

        {!isRoot && (
          <input
            type="checkbox"
            checked={isSelected}
            onChange={() => onToggle(id)}
            aria-label={`跳过目录 ${name}`}
          />
        )}

        <span
          style={{
            fontSize: "13px",
            cursor: isRoot ? "default" : "pointer",
            userSelect: "none",
            fontWeight: isRoot ? 600 : 400,
          }}
          onClick={isRoot ? undefined : handleToggleOpen}
        >
          {name}
          {isRoot ? " (根目录)" : ""}
        </span>
        {!isRoot && (
          <span
            className="admin-mono-cell admin-text-faint"
            style={{ fontSize: "11px", marginLeft: "6px" }}
          >
            {id}
          </span>
        )}
      </div>

      {open && (
        <div>
          {loading && (
            <div className="admin-text-faint" style={{ fontSize: "12px", padding: "4px 28px" }}>
              加载中...
            </div>
          )}
          {error && (
            <div style={{ fontSize: "12px", padding: "4px 28px", color: "var(--danger, #d33)" }}>
              {error}
            </div>
          )}
          {loaded && !error && children.length === 0 && (
            <div className="admin-text-faint" style={{ fontSize: "12px", padding: "4px 28px" }}>
              （无子目录）
            </div>
          )}
          {children.map((child) => (
            <DirTreeNode
              key={child.id}
              driveId={driveId}
              id={child.id}
              name={child.name}
              depth={depth + 1}
              ancestorSkipped={ancestorSkipped || isSelected}
              selected={selected}
              onToggle={onToggle}
            />
          ))}
        </div>
      )}
    </div>
  );
}
