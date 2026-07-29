"use client";

import { useEffect } from "react";
import { cn } from "@/lib/utils";

// ── Button ──
type ButtonVariant = "primary" | "ghost" | "danger" | "outline";

const BTN_VARIANTS: Record<ButtonVariant, string> = {
  primary: "bg-zinc-900 text-white hover:bg-zinc-700",
  outline: "border border-zinc-300 bg-white text-zinc-700 hover:bg-zinc-50",
  ghost: "text-zinc-600 hover:bg-zinc-100 hover:text-zinc-900",
  danger: "border border-red-200 bg-white text-red-600 hover:bg-red-50",
};

export function Button({
  variant = "primary",
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { variant?: ButtonVariant }) {
  return (
    <button
      className={cn(
        "inline-flex items-center justify-center px-3 py-1.5 text-sm font-medium rounded-md transition-colors disabled:opacity-50 disabled:cursor-not-allowed",
        BTN_VARIANTS[variant],
        className
      )}
      {...props}
    />
  );
}

// ── Badge ──
const STATUS_COLORS: Record<string, string> = {
  // Goal statuses
  backlog: "bg-zinc-100 text-zinc-600",
  active: "bg-blue-50 text-blue-700 ring-1 ring-blue-200",
  blocked: "bg-amber-50 text-amber-700 ring-1 ring-amber-200",
  done: "bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200",
  failed: "bg-red-50 text-red-700 ring-1 ring-red-200",
  cancelled: "bg-zinc-100 text-zinc-400",
  // Run statuses
  queued: "bg-zinc-100 text-zinc-600",
  running: "bg-green-50 text-green-700 ring-1 ring-green-200",
  completed: "bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200",
};

export function Badge({ status, className }: { status: string; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex items-center px-2 py-0.5 text-xs font-medium rounded",
        STATUS_COLORS[status] ?? "bg-zinc-100 text-zinc-600",
        className
      )}
    >
      {status}
    </span>
  );
}

// ── Form primitives ──
export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <label className="block space-y-1">
      <span className="text-xs font-medium text-zinc-500">{label}</span>
      {children}
      {hint && <span className="block text-xs text-zinc-400">{hint}</span>}
    </label>
  );
}

export const inputCls =
  "w-full px-3 py-2 border border-zinc-300 rounded-md text-sm bg-white text-zinc-900 placeholder:text-zinc-400 focus:border-zinc-500 focus:ring-2 focus:ring-zinc-200 outline-none transition";

// ── Dialog ──
export function Dialog({
  title,
  onClose,
  children,
  footer,
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  footer: React.ReactNode;
}) {
  // Esc to close.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-zinc-900/40 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-md bg-white rounded-xl shadow-2xl border border-zinc-200 flex flex-col max-h-[90vh]"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-zinc-100">
          <h2 className="text-base font-semibold text-zinc-900">{title}</h2>
          <button
            onClick={onClose}
            className="text-zinc-400 hover:text-zinc-700 text-lg leading-none px-1"
            aria-label="关闭"
          >
            ×
          </button>
        </div>
        <div className="p-5 space-y-4 overflow-y-auto">{children}</div>
        <div className="flex justify-end gap-2 px-5 py-3.5 border-t border-zinc-100 bg-zinc-50/50">
          {footer}
        </div>
      </div>
    </div>
  );
}

// ── Confirm Dialog ──
export function ConfirmDialog({
  title,
  message,
  onConfirm,
  onClose,
  loading,
}: {
  title: string;
  message: string;
  onConfirm: () => void;
  onClose: () => void;
  loading?: boolean;
}) {
  return (
    <Dialog
      title={title}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button variant="danger" onClick={onConfirm} disabled={loading}>
            {loading ? "…" : "确认删除"}
          </Button>
        </>
      }
    >
      <p className="text-sm text-zinc-600">{message}</p>
    </Dialog>
  );
}

// ── Page header ──
export function PageHeader({
  title,
  action,
}: {
  title: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between mb-5">
      <h1 className="text-lg font-semibold text-zinc-900">{title}</h1>
      {action}
    </div>
  );
}

// ── Empty state ──
export function Empty({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center py-16 text-center">
      <p className="text-sm text-zinc-400">{children}</p>
    </div>
  );
}

// ── Skeleton ──
export function Skeleton({ className }: { className?: string }) {
  return (
    <div
      className={cn(
        "animate-pulse rounded-md bg-zinc-100",
        className
      )}
    />
  );
}
