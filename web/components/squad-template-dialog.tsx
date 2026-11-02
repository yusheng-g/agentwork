"use client";

import { useEffect, useMemo, useState } from "react";
import { ChevronDown, RefreshCw, Users, CheckCircle2, XCircle, Loader2, Search } from "lucide-react";
import { Button, Dialog, Field, inputCls, Empty } from "@/components/ui";
import { cn } from "@/lib/utils";
import { useTemplates, useTemplate, useRefreshTemplates, useApplySquadTemplate } from "@/lib/queries";
import type { TemplateSummary, ApplyItem, ApplySquadOverrides, SquadTemplateSpec, TplAgent } from "@/lib/types";

// 小队模板 apply 弹窗：列表 → 选模板 → 预览（YAML + 将创建摘要）→ 覆盖表单 → 结果。
// 行为对齐 AI_Shell_WEB/SquadTemplateDialog.vue。
export function SquadTemplateDialog({ onClose }: { onClose: () => void }) {
  const { data: templates, isLoading: loadingList } = useTemplates("squad");
  const refresh = useRefreshTemplates();
  const [search, setSearch] = useState("");
  const [selectedId, setSelectedId] = useState<string>("");
  const [showYaml, setShowYaml] = useState(false);

  const detail = useTemplate("squad", selectedId, !!selectedId);
  const spec = (detail.data?.spec as SquadTemplateSpec | undefined) ?? null;
  const rawYaml = detail.data?.spec_yaml ?? "";

  // 覆盖表单状态
  const [squadName, setSquadName] = useState("");
  const [agentNames, setAgentNames] = useState<Record<string, string>>({});

  // 结果状态
  const [resultItems, setResultItems] = useState<ApplyItem[] | null>(null);
  const [resultSquadName, setResultSquadName] = useState("");
  const [applyError, setApplyError] = useState("");

  const apply = useApplySquadTemplate();

  // specAgents 必须稳定——`?? []` 每次渲染都分配新数组，放 effect deps
  // 会造成无限重渲染循环（Maximum update depth exceeded）。useMemo 钉在 spec 上。
  const specAgents: TplAgent[] = useMemo(() => spec?.agents ?? [], [spec]);
  const specSquad = spec?.squad;

  useEffect(() => {
    setSquadName(specSquad?.name ?? "");
    const map: Record<string, string> = {};
    for (const a of specAgents) if (a.name) map[a.name] = "";
    setAgentNames(map);
  }, [specSquad, specAgents]);

  const filtered = useMemo(() => {
    if (!templates) return [];
    const q = search.trim().toLowerCase();
    if (!q) return templates;
    return templates.filter((t) =>
      [t.name, t.description, ...(t.tags ?? [])].join(" ").toLowerCase().includes(q)
    );
  }, [templates, search]);

  const renamePayload = useMemo(() => {
    const out: Record<string, string> = {};
    for (const [tpl, override] of Object.entries(agentNames)) {
      const v = override.trim();
      if (v && v !== tpl) out[tpl] = v;
    }
    return out;
  }, [agentNames]);

  const canSubmit = !!selectedId && squadName.trim() !== "";
  const step: "select" | "done" = resultItems || applyError ? "done" : "select";

  function reset() {
    setResultItems(null);
    setResultSquadName("");
    setApplyError("");
    setSelectedId("");
    setShowYaml(false);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!canSubmit || apply.isPending) return;
    setApplyError("");
    setResultItems(null);
    const body: ApplySquadOverrides = {
      squad_name: squadName.trim(),
      rename: Object.keys(renamePayload).length ? renamePayload : undefined,
    };
    apply.mutate(
      { id: selectedId, body },
      {
        onSuccess: (res) => {
          setResultItems(res.items ?? []);
          setResultSquadName(res.squad?.name ?? squadName.trim());
        },
        onError: (err) => setApplyError(String(err)),
      }
    );
  }

  return (
    <Dialog
      title="从小队模板创建"
      onClose={onClose}
      wide
      footer={
        step === "done" ? (
          <>
            <Button variant="outline" onClick={onClose}>关闭</Button>
            <Button onClick={reset}>继续创建</Button>
          </>
        ) : (
          <>
            {applyError && (
              <span className="mr-auto text-xs text-red-500 max-w-[60%] truncate" title={applyError}>
                {applyError}
              </span>
            )}
            <Button variant="outline" onClick={onClose}>取消</Button>
            <Button type="submit" form="squad-tpl-form" disabled={!canSubmit || apply.isPending}>
              {apply.isPending ? (
                <span className="flex items-center gap-1.5"><Loader2 className="h-3.5 w-3.5 animate-spin" />创建中…</span>
              ) : (
                "创建小队"
              )}
            </Button>
          </>
        )
      }
    >
      {step === "done" ? (
        <DoneView items={resultItems ?? []} squadName={resultSquadName} error={applyError} />
      ) : (
        <div className="space-y-4">
          <div className="flex items-center gap-2">
            <div className="relative flex-1">
              <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-zinc-400" />
              <input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="搜索模板…"
                className={cn(inputCls, "pl-8 py-1.5")}
              />
            </div>
            <Button
              variant="outline"
              onClick={() => refresh.mutate(undefined, { onSuccess: () => detail.refetch() })}
              disabled={refresh.isPending}
              title="从模板仓库重新拉取"
            >
              <RefreshCw className={cn("h-3.5 w-3.5", refresh.isPending && "animate-spin")} />
              刷新
            </Button>
          </div>

          {loadingList ? (
            <div className="text-sm text-zinc-400 py-10 text-center">加载模板中…</div>
          ) : !filtered.length ? (
            <Empty icon="📦">暂无可用小队模板 — 点击「刷新」从模板仓库拉取</Empty>
          ) : (
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
              {filtered.map((t) => (
                <SquadCard
                  key={t.id}
                  t={t}
                  selected={selectedId === t.id}
                  onSelect={() => setSelectedId(t.id)}
                />
              ))}
            </div>
          )}

          {selectedId && (
            <form id="squad-tpl-form" onSubmit={handleSubmit} className="space-y-4">
              {detail.isLoading ? (
                <div className="text-sm text-zinc-400 py-4 text-center">加载模板详情…</div>
              ) : spec ? (
                <>
                  <div>
                    <button
                      type="button"
                      onClick={() => setShowYaml((v) => !v)}
                      className="flex items-center gap-1 text-xs font-medium text-zinc-500 hover:text-zinc-700"
                    >
                      <ChevronDown className={cn("h-3.5 w-3.5 transition-transform", showYaml && "rotate-180")} />
                      {showYaml ? "收起" : "查看"}模板 YAML
                    </button>
                    {showYaml && (
                      <pre className="mt-2 max-h-48 overflow-auto rounded-lg bg-zinc-50 p-3 text-[11px] leading-relaxed text-zinc-600 whitespace-pre-wrap break-all">
                        {rawYaml || "（详情加载中…）"}
                      </pre>
                    )}
                  </div>

                  <PreviewBox spec={spec} />

                  <Field label="小队名称" hint="与平台已有小队重名时按名更新（upsert）">
                    <input
                      value={squadName}
                      onChange={(e) => setSquadName(e.target.value)}
                      className={inputCls}
                      placeholder="小队名…"
                      required
                    />
                  </Field>

                  {specAgents.length > 0 && (
                    <fieldset className="border border-zinc-200 rounded-lg p-3 space-y-2">
                      <legend className="text-xs font-medium text-zinc-500 px-1">
                        Agent 命名（留空 = 用模板默认名）
                      </legend>
                      {specAgents.map((a) => (
                        <div key={a.name} className="flex items-center gap-2">
                          <span className="w-28 shrink-0 truncate text-xs text-zinc-500" title={a.name}>{a.name}</span>
                          <input
                            value={agentNames[a.name] ?? ""}
                            onChange={(e) => setAgentNames((m) => ({ ...m, [a.name]: e.target.value }))}
                            className={inputCls}
                            placeholder={`默认：${a.name}`}
                          />
                        </div>
                      ))}
                    </fieldset>
                  )}
                </>
              ) : null}
            </form>
          )}
        </div>
      )}
    </Dialog>
  );
}

function SquadCard({
  t,
  selected,
  onSelect,
}: {
  t: TemplateSummary;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={cn(
        "text-left p-3 rounded-lg border transition",
        selected
          ? "border-indigo-400 bg-indigo-50/60 ring-1 ring-indigo-200"
          : "border-zinc-200 hover:border-zinc-300 hover:bg-zinc-50"
      )}
    >
      <div className="flex items-start gap-2">
        {t.icon && <span className="text-lg leading-none">{t.icon}</span>}
        <div className="flex-1 min-w-0">
          <div className="flex items-center justify-between gap-2">
            <span className="font-medium text-sm text-zinc-900 truncate">{t.name}</span>
            <span className="text-[10px] text-zinc-400 shrink-0">v{t.version}</span>
          </div>
          <p className="text-xs text-zinc-500 line-clamp-2 mt-0.5">{t.description}</p>
          {t.tags?.length ? (
            <div className="mt-1.5 flex flex-wrap gap-1">
              {t.tags.map((tag) => (
                <span key={tag} className="rounded bg-zinc-100 px-1.5 py-0.5 text-[10px] text-zinc-500">{tag}</span>
              ))}
            </div>
          ) : null}
        </div>
      </div>
    </button>
  );
}

function PreviewBox({ spec }: { spec: SquadTemplateSpec }) {
  const squad = spec.squad;
  const members = squad?.members ?? [];
  const skills = Array.from(
    new Set(spec.agents.flatMap((a) => a.skills ?? []))
  );

  return (
    <div className="border border-zinc-200 rounded-lg p-3 space-y-2 bg-zinc-50/40">
      <div className="flex items-center gap-1.5 text-xs font-medium text-zinc-700">
        <Users className="h-3.5 w-3.5" />
        将创建
      </div>
      <p className="text-xs text-zinc-600">
        Leader：{squad?.leader || "—"}
        {members.length ? ` ｜ 成员：${members.map((m) => m.name).join("、")}` : ""}
        {skills.length ? ` ｜ 引用 skill：${skills.join("、")}` : ""}
      </p>
      <div className="space-y-1.5">
        {spec.agents.map((a) => (
          <div key={a.name} className="text-xs">
            <span className="font-medium text-zinc-800">
              {a.name}
              {a.name === squad?.leader && (
                <span className="ml-1.5 inline-flex items-center px-1.5 py-0.5 rounded bg-indigo-50 text-indigo-600 text-[10px]">leader</span>
              )}
            </span>
            {a.description && <p className="text-zinc-500 line-clamp-1">{a.description}</p>}
          </div>
        ))}
      </div>
    </div>
  );
}

const ACTION_LABELS: Record<string, string> = {
  created: "创建",
  updated: "更新",
  skipped: "跳过",
  failed: "失败",
};

function DoneView({
  items,
  squadName,
  error,
}: {
  items: ApplyItem[];
  squadName: string;
  error: string;
}) {
  const hasFailure = items.some((it) => it.action === "failed");
  const success = !error && !hasFailure;
  return (
    <div className="space-y-3">
      <div
        className={cn(
          "rounded-lg p-3 text-sm",
          success ? "bg-emerald-50 text-emerald-700" : "bg-amber-50 text-amber-700"
        )}
      >
        {success ? (
          <span>小队「{squadName}」创建成功</span>
        ) : (
          <div>
            <p>小队模板应用完成（含失败项）</p>
            <p className="text-xs mt-1 opacity-80">
              失败项可修正后重试：默认策略为 upsert，已成功项不会重复创建。
            </p>
          </div>
        )}
      </div>
      {error && <p className="text-xs text-red-500">{error}</p>}
      {items.length > 0 && (
        <ul className="space-y-1.5">
          {items.map((it, i) => {
            const failed = it.action === "failed";
            return (
              <li key={i} className="flex items-start gap-2 text-sm">
                {failed ? (
                  <XCircle className="h-4 w-4 text-red-500 mt-0.5 shrink-0" />
                ) : (
                  <CheckCircle2 className="h-4 w-4 text-emerald-500 mt-0.5 shrink-0" />
                )}
                <span className="font-medium text-zinc-800">{it.name}</span>
                <span className="text-xs text-zinc-400">{it.kind}</span>
                <span className={cn("ml-auto text-xs", failed ? "text-red-500" : "text-zinc-500")}>
                  {ACTION_LABELS[it.action] ?? it.action}
                  {it.error ? `：${it.error}` : ""}
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
