import {
  ReactNode,
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";

type ToastKind = "info" | "success" | "error";
type Toast = { id: number; kind: ToastKind; text: string };

type Ctx = {
  show: (text: string, kind?: ToastKind) => void;
};

const ToastCtx = createContext<Ctx | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const timers = useRef<Record<string, ReturnType<typeof window.setTimeout>>>({});

  // Deduplicate: same text won't stack, just resets the dismiss timer
  const show = useCallback((text: string, kind: ToastKind = "info") => {
    // Reset timer if duplicate
    if (timers.current[text]) {
      window.clearTimeout(timers.current[text]);
      timers.current[text] = window.setTimeout(() => {
        setItems((list) => list.filter((t) => t.text !== text));
        delete timers.current[text];
      }, 2600);
      return;
    }
    const id = Date.now() + Math.random();
    timers.current[text] = window.setTimeout(() => {
      setItems((list) => list.filter((t) => t.id !== id));
      delete timers.current[text];
    }, 2600);
    setItems((list) => [...list, { id, kind, text }]);
  }, []);

  return (
    <ToastCtx.Provider value={{ show }}>
      {children}
      {items.map((t) => (
        <div key={t.id} className={`admin-toast is-${t.kind}`}>
          {t.text}
        </div>
      ))}
    </ToastCtx.Provider>
  );
}

export function useToast(): Ctx {
  const ctx = useContext(ToastCtx);
  if (!ctx) throw new Error("useToast must be used inside <ToastProvider>");
  return ctx;
}

// 小工具：自动关闭的 toast 倒计时，用于某些异步提示展示后返回
export function useFlashError(): [string | null, (msg: string | null) => void] {
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    if (!err) return;
    const t = window.setTimeout(() => setErr(null), 4000);
    return () => window.clearTimeout(t);
  }, [err]);
  return [err, setErr];
}
