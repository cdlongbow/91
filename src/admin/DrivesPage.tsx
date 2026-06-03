import { useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowLeft,
  ChevronRight,
  Download,
  FolderTree,
  HardDrive,
  PlayCircle,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import * as api from "./api";
import { useToast } from "./ToastContext";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import { formatBytes } from "./storageFormat";
import { makeUniqueDriveId } from "./driveId";
import {
  FormState,
  kindLabel,
  emptyForm,
  idleNightlyStatus,
  nightlyButtonText,
  nightlyBusyText,
  usesRootDirectoryID,
  defaultRootId,
} from "./drive/constants";
import {
  StorageSummary,
  StatusTag,
  DriveCardMetrics,
  DriveGenerationPanel,
} from "./drive/DriveComponents";
import { DriveForm } from "./drive/DriveForm";
import { DeleteDriveModal } from "./drive/DeleteDriveModal";
import { SkipDirsPanel } from "./drive/SkipDirsPanel";

export function DrivesPage() {
  const [list, setList] = useState<api.AdminDrive[]>([]);
  const [storage, setStorage] = useState<api.AdminDriveStorage | null>(null);
  const [settings, setSettings] = useState<api.Settings | null>(null);
  const [nightlyStatus, setNightlyStatus] =
    useState<api.NightlyJobStatus>(idleNightlyStatus);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [modalOpen, setModalOpen] = useState(false);
  const [discardConfirmOpen, setDiscardConfirmOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<api.AdminDrive | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [initialForm, setInitialForm] = useState<FormState>(emptyForm);
  const [nameTouched, setNameTouched] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deletingId, setDeletingId] = useState("");
  const [regenFailedId, setRegenFailedId] = useState("");
  const [regenFailedThumbId, setRegenFailedThumbId] = useState("");
  const [regenFailedFingerprintId, setRegenFailedFingerprintId] = useState("");
  const [togglingTeaserId, setTogglingTeaserId] = useState("");
  const [scanningAll, setScanningAll] = useState(false);
  const [trackingNightly, setTrackingNightly] = useState(false);
  const [scanningDriveId, setScanningDriveId] = useState("");
  const [selectedDriveId, setSelectedDriveId] = useState<string | null>(null);
  const { show } = useToast();
  const pollConnectionLost = useRef(false);
  const nightlyBusy = scanningAll || nightlyStatus.running || nightlyStatus.queued;
  const nameMissing = form.name.trim().length === 0;
  const nameError = nameTouched && nameMissing ? "请填写网盘名称" : "";
  const formDirty = !sameForm(form, initialForm);

  const uploadTargets = useMemo(
    () => list.filter((d) => d.kind === "pikpak" || d.kind === "p115" || d.kind === "p123" || d.kind === "onedrive"),
    [list]
  );

  async function refresh() {
    setLoading(true);
    setLoadError("");
    try {
      const [data, storageData, settingsData, jobStatus] = await Promise.all([
        api.listDrives(),
        api.getDriveStorage(),
        api.getSettings().catch(() => null),
        api.getNightlyJobStatus().catch(() => null),
      ]);
      setList(data ?? []);
      setStorage(storageData);
      if (settingsData) setSettings(settingsData);
      if (jobStatus) setNightlyStatus(jobStatus);
    } catch (e) {
      const message = e instanceof Error ? e.message : "加载失败";
      setLoadError(message);
      show(message, "error");
    } finally {
      setLoading(false);
    }
  }

  async function refreshDriveList() {
    try {
      const [data, jobStatus] = await Promise.all([
        api.listDrives(),
        api.getNightlyJobStatus().catch(() => null),
      ]);
      setList(data ?? []);
      if (jobStatus) setNightlyStatus(jobStatus);
      if (pollConnectionLost.current) {
        pollConnectionLost.current = false;
        show("连接已恢复，网盘数据已更新", "success");
      }
    } catch {
      if (!pollConnectionLost.current) {
        pollConnectionLost.current = true;
        show("连接中断，网盘数据可能不是最新", "error");
      }
    }
  }

  useEffect(() => {
    refresh();
  }, []);

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden && !modalOpen) {
        refreshDriveList();
      }
    }, 5000);
    return () => window.clearInterval(timer);
  }, [modalOpen]);

  useEffect(() => {
    if (!trackingNightly) return;
    const timer = window.setInterval(async () => {
      try {
        const status = await api.getNightlyJobStatus();
        setNightlyStatus(status);
        if (status.running || (!status.queued && !status.running)) {
          setTrackingNightly(false);
        }
      } catch {
        // The normal drive polling already reports connection loss.
      }
    }, 2000);
    return () => window.clearInterval(timer);
  }, [trackingNightly]);

  function openCreate() {
    const nextForm = {
      ...emptyForm,
      spider91UploadDriveId: settings?.spider91UploadDriveId ?? "",
    };
    setForm(nextForm);
    setInitialForm(nextForm);
    setNameTouched(false);
    setModalOpen(true);
  }

  function openEdit(d: api.AdminDrive) {
    const nextForm: FormState = {
      id: d.id,
      kind: d.kind,
      name: d.name,
      rootId: d.rootId,
      creds: d.kind === "spider91" ? { proxy: d.spider91Proxy ?? "" } : {},
      spider91UploadDriveId: settings?.spider91UploadDriveId ?? "",
    };
    setForm(nextForm);
    setInitialForm(nextForm);
    setNameTouched(false);
    setModalOpen(true);
  }

  function requestCloseDriveModal() {
    if (saving) return;
    if (formDirty) {
      setDiscardConfirmOpen(true);
      return;
    }
    setModalOpen(false);
  }

  function discardDriveChanges() {
    setDiscardConfirmOpen(false);
    setModalOpen(false);
    setForm(initialForm);
    setNameTouched(false);
  }

  async function handleSave() {
    const name = form.name.trim();
    if (!name || !form.kind) {
      setNameTouched(true);
      show("请填名称和类型", "error");
      return;
    }
    const existing = list.find((x) => x.id === form.id);
    const driveID = existing
      ? form.id
      : makeUniqueDriveId(form.kind, name, list);
    const rootId = usesRootDirectoryID(form.kind)
      ? form.rootId.trim() || defaultRootId(form.kind)
      : defaultRootId(form.kind);
    setSaving(true);
    try {
      const resp = await api.upsertDrive({
        id: driveID,
        kind: form.kind,
        name,
        rootId,
        credentials: form.creds,
      });

      if (form.kind === "spider91" && form.spider91UploadDriveId !== (settings?.spider91UploadDriveId ?? "")) {
        try {
          const updated = await api.updateSettings({
            spider91UploadDriveId: form.spider91UploadDriveId,
          });
          setSettings(updated);
        } catch (settingsErr) {
          show(
            settingsErr instanceof Error
              ? `Drive 已保存，但上传目标设置失败：${settingsErr.message}`
              : "上传目标设置失败",
            "error"
          );
          setModalOpen(false);
          setInitialForm(form);
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
      setInitialForm(form);
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "error");
    } finally {
      setSaving(false);
    }
  }

  async function confirmDeleteDrive() {
    if (!deleteTarget) return;
    const d = deleteTarget;
    setDeletingId(d.id);
    try {
      const resp = await api.deleteDrive(d.id, { deleteVideos: true });
      show(`已删除，并清理 ${resp.deletedVideos ?? 0} 个视频`, "success");
      setDeleteTarget(null);
      if (selectedDriveId === d.id) {
        setSelectedDriveId(null);
      }
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "删除失败", "error");
    } finally {
      setDeletingId("");
    }
  }

  async function handleRescan(d: api.AdminDrive) {
    if (scanningDriveId) return;
    setScanningDriveId(d.id);
    try {
      await api.rescan(d.id);
      if (d.kind === "spider91") {
        show("已触发抓取任务，需要 2-4 分钟，可稍后刷新视频列表查看", "success");
      } else {
        show("已触发扫描，可稍后刷新视频列表查看", "success");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setScanningDriveId("");
    }
  }

  async function handleRunNightly() {
    if (nightlyBusy) {
      show(nightlyBusyText(nightlyStatus) || "当前已有扫描所有网盘任务", "info");
      return;
    }
    setScanningAll(true);
    try {
      const resp = await api.runNightlyJob();
      setNightlyStatus(resp.status);
      if (resp.accepted) {
        setTrackingNightly(!resp.status.running);
        show("已触发扫描所有网盘，耗时较长，可在任务状态和 backend 日志观察进度", "success");
      } else {
        show("当前已有扫描所有网盘任务", "info");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setScanningAll(false);
    }
  }

  async function handleRegenFailed(d: api.AdminDrive) {
    setRegenFailedId(d.id);
    try {
      await api.regenFailedPreviews(d.id);
      show("已触发失败预览视频重新生成", "success");
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setRegenFailedId("");
    }
  }

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

  async function handleRegenFailedFingerprints(d: api.AdminDrive) {
    setRegenFailedFingerprintId(d.id);
    try {
      await api.regenFailedFingerprints(d.id);
      show("已触发失败指纹重新生成", "success");
      refresh();
    } catch (e) {
      show(e instanceof Error ? e.message : "触发失败", "error");
    } finally {
      setRegenFailedFingerprintId("");
    }
  }

  async function handleToggleTeaser(d: api.AdminDrive) {
    const next = !d.teaserEnabled;
    setTogglingTeaserId(d.id);
    setList((prev) =>
      prev.map((item) =>
        item.id === d.id ? { ...item, teaserEnabled: next } : item
      )
    );
    try {
      const resp = await api.setDriveTeaserEnabled(d.id, next);
      show(
        resp.teaserEnabled
          ? `已开启「${d.name || d.id}」的预览视频生成`
          : `已关闭「${d.name || d.id}」的预览视频生成`,
        "success"
      );
      setList((prev) =>
        prev.map((item) =>
          item.id === d.id ? { ...item, teaserEnabled: resp.teaserEnabled } : item
        )
      );
      refreshDriveList();
    } catch (e) {
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

  // --- Detail view ---
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
                {usesRootDirectoryID(d.kind) && (
                  <div className="admin-detail-row">
                    <span className="admin-detail-label">根目录 ID</span>
                    <span className="admin-detail-value admin-mono-cell">{d.rootId}</span>
                  </div>
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
                <button
                  type="button"
                  className="admin-btn is-primary"
                  onClick={() => handleRescan(d)}
                  disabled={!!scanningDriveId}
                >
                  {d.kind === "spider91" ? (
                    <>
                      <Download size={13} className={scanningDriveId === d.id ? "admin-spin" : undefined} />
                      {scanningDriveId === d.id ? "触发中..." : "立即抓取"}
                    </>
                  ) : (
                    <>
                      <RefreshCw size={13} className={scanningDriveId === d.id ? "admin-spin" : undefined} />
                      {scanningDriveId === d.id ? "触发中..." : "立即重扫"}
                    </>
                  )}
                </button>
                <button type="button" className="admin-btn" onClick={() => openEdit(d)}>
                  {d.kind === "spider91" ? "编辑配置" : "编辑配置凭证"}
                </button>
                <button type="button" className="admin-btn is-danger" onClick={() => setDeleteTarget(d)} style={{ marginLeft: "auto" }}>
                  <Trash2 size={13} /> 删除网盘
                </button>
              </div>
            </div>

            {d.kind !== "spider91" && (
              <SkipDirsPanel
                drive={d}
                onSaved={(saved) => {
                  setList((prev) =>
                    prev.map((item) =>
                      item.id === saved.id ? { ...item, skipDirIds: saved.skipDirIds } : item
                    )
                  );
                  refreshDriveList();
                }}
              />
            )}
          </div>

          <div>
            <DriveGenerationPanel
              d={d}
              regenFailedId={regenFailedId}
              regenFailedThumbId={regenFailedThumbId}
              regenFailedFingerprintId={regenFailedFingerprintId}
              togglingTeaserId={togglingTeaserId}
              onToggleTeaser={() => handleToggleTeaser(d)}
              onRegenFailed={() => handleRegenFailed(d)}
              onRegenFailedThumbnails={() => handleRegenFailedThumbnails(d)}
              onRegenFailedFingerprints={() => handleRegenFailedFingerprints(d)}
            />

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
                  <span className="admin-detail-label">预览视频占用</span>
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
          onClose={requestCloseDriveModal}
          footer={
            <>
              <button type="button" className="admin-btn" onClick={requestCloseDriveModal}>
                取消
              </button>
              <button
                type="button"
                className="admin-btn is-primary"
                onClick={handleSave}
                disabled={saving || nameMissing}
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
            nameError={nameError}
            onNameBlur={() => setNameTouched(true)}
          />
        </Modal>
        <DeleteDriveModal
          drive={deleteTarget}
          deleting={deletingId === deleteTarget?.id}
          onCancel={() => {
            if (!deletingId) {
              setDeleteTarget(null);
            }
          }}
          onConfirm={confirmDeleteDrive}
        />
        <ConfirmModal
          open={discardConfirmOpen}
          title="放弃未保存更改"
          message="当前网盘配置有未保存的更改，确定要放弃吗？"
          confirmText="放弃更改"
          danger
          onCancel={() => setDiscardConfirmOpen(false)}
          onConfirm={discardDriveChanges}
        />
      </section>
    );
  }

  // --- List view ---
  return (
    <section>
      <header className="admin-page__header">
        <h1 className="admin-page__title">网盘管理</h1>
        <div style={{ display: "flex", gap: "8px" }}>
          <button
            type="button"
            className="admin-btn"
            onClick={handleRunNightly}
            disabled={scanningAll}
            title={nightlyBusyText(nightlyStatus) || "立即扫描所有网盘。耗时较长，期间不要重复触发。"}
          >
            <PlayCircle size={14} /> {nightlyButtonText(nightlyStatus, scanningAll)}
          </button>
          <button type="button" className="admin-btn is-primary" onClick={openCreate}>
            <Plus size={14} /> 新建网盘
          </button>
        </div>
      </header>

      {storage && <StorageSummary storage={storage} />}

      {loading ? (
        <div className="admin-loading-state">
          <RefreshCw size={20} className="admin-spin" />
          <span>加载中...</span>
        </div>
      ) : loadError ? (
        <div className="admin-error-state">
          <strong>网盘数据加载失败</strong>
          <span>{loadError}</span>
          <button type="button" className="admin-btn" onClick={refresh}>
            <RefreshCw size={13} /> 重试
          </button>
        </div>
      ) : list.length === 0 ? (
        <div className="admin-card admin-empty">
          还没有配置任何网盘。点击右上角「新建」，选择夸克 / 115 / PikPak / 沃盘 / OneDrive / 本地存储，填入凭证或路径即可。
        </div>
      ) : (
        <div className="admin-drives-grid">
          {list.map((d) => (
            <button
              type="button"
              key={d.id}
              className="admin-drive-card"
              onClick={() => setSelectedDriveId(d.id)}
              aria-label={`管理网盘 ${d.name || d.id}`}
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

              <DriveCardMetrics d={d} />

              <div className="admin-drive-card__footer">
                <span>本地占用: {formatBytes(storage?.drives[d.id]?.totalBytes ?? 0)}</span>
                <span className="admin-drive-card__manage-link">
                  管理 <ChevronRight size={14} />
                </span>
              </div>
            </button>
          ))}
        </div>
      )}

      <Modal
        open={modalOpen}
        title={form.id && list.find((x) => x.id === form.id) ? "编辑网盘" : "新建网盘"}
        onClose={requestCloseDriveModal}
        footer={
          <>
            <button type="button" className="admin-btn" onClick={requestCloseDriveModal}>
              取消
            </button>
            <button
              type="button"
              className="admin-btn is-primary"
              onClick={handleSave}
              disabled={saving || nameMissing}
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
          nameError={nameError}
          onNameBlur={() => setNameTouched(true)}
          onBack={() => setNameTouched(false)}
        />
      </Modal>
      <DeleteDriveModal
        drive={deleteTarget}
        deleting={deletingId === deleteTarget?.id}
        onCancel={() => {
          if (!deletingId) {
            setDeleteTarget(null);
          }
        }}
        onConfirm={confirmDeleteDrive}
      />
      <ConfirmModal
        open={discardConfirmOpen}
        title="放弃未保存更改"
        message="当前网盘配置有未保存的更改，确定要放弃吗？"
        confirmText="放弃更改"
        danger
        onCancel={() => setDiscardConfirmOpen(false)}
        onConfirm={discardDriveChanges}
      />
    </section>
  );
}

function sameForm(a: FormState, b: FormState): boolean {
  return (
    a.id === b.id &&
    a.kind === b.kind &&
    a.name === b.name &&
    a.rootId === b.rootId &&
    a.spider91UploadDriveId === b.spider91UploadDriveId &&
    sameRecord(a.creds, b.creds)
  );
}

function sameRecord(a: Record<string, string>, b: Record<string, string>): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const key of keys) {
    if ((a[key] ?? "") !== (b[key] ?? "")) return false;
  }
  return true;
}
