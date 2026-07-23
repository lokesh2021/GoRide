import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { ApiError } from "../api/client";

type ToastKind = "error" | "success" | "info";

interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
  code?: string;
}

interface ToastCtx {
  push: (kind: ToastKind, message: string, code?: string) => void;
  error: (err: unknown, fallback?: string) => void;
  success: (message: string) => void;
  info: (message: string) => void;
}

const Ctx = createContext<ToastCtx | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const seq = useRef(0);

  const push = useCallback((kind: ToastKind, message: string, code?: string) => {
    const id = ++seq.current;
    setToasts((t) => [...t, { id, kind, message, code }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 4500);
  }, []);

  const error = useCallback(
    (err: unknown, fallback = "Something went wrong") => {
      // Surface the backend error envelope message + stable code.
      if (err instanceof ApiError) push("error", err.message, err.code);
      else if (err instanceof Error) push("error", err.message || fallback);
      else push("error", fallback);
    },
    [push],
  );

  const success = useCallback((m: string) => push("success", m), [push]);
  const info = useCallback((m: string) => push("info", m), [push]);

  return (
    <Ctx.Provider value={{ push, error, success, info }}>
      {children}
      <div className="toasts">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.kind}`}>
            {t.code && <span className="code">{t.code}</span>}
            {t.message}
          </div>
        ))}
      </div>
    </Ctx.Provider>
  );
}

export function useToast(): ToastCtx {
  const c = useContext(Ctx);
  if (!c) throw new Error("useToast must be used within ToastProvider");
  return c;
}
