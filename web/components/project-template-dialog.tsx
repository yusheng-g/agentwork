"use client";

import { useEffect, useMemo, useState } from "react";
import { ChevronDown, RefreshCw, FolderGit2, Users, ListChecks, CheckCircle2, XCircle, Loader2, FileBox, Search } from "lucide-react";
import { Button, Dialog, Field, inputCls, Empty } from "@/components/ui";
import { cn } from "@/lib/utils";
import { useTemplates, useTemplate, useRefreshTemplates, useApplyProjectTemplate } from "@/lib/queries";
import type { TemplateSummary, ApplyItem, ApplyProjectOverrides, ProjectTemplateSpec, TplAgent } from "@/lib/types";

// 项目模板 apply 弹窗：列表 → 选模板 → 预览（YAML + 将创建摘要）→ 覆盖表单 → 结果。
// 行为对齐 AI_Shell_WEB/ProjectTemplateDialog.vue，仅把 Vue 的两步式状态机
// 映射为 React 的 useState + 派生 step。
export function ProjectTemplateDialog({ onClose }: { onClose: () => void }) {
  const { data: templates, isLoading: loadingList } = useTemplates("project");
  const refresh = useRefreshTemplates();
  const [search, setSearch] = useState("");
  const [selectedId, setSelectedId] = useState<string>("");
  const [showYaml, setShowYaml] = useState(false);

  const detail = useTemplate("project", selectedId, !!selectedId);
  const spec = (detail.data?.spec as ProjectTemplateSpec | undefined) ?? null;
  const rawYaml = detail.data?.spec_yaml ?? "";

  // 覆盖表单状态
  const [projectName, setProjectName] = useState("");
  const [repoSource, setRepoSource] = useState<"create" | "existing">("existing");
  const [gitUrl, setGitUrl] = useState("");
  const [gitCredentials, setGitCredentials] = useState("");
  const [repoToken, setRepoToken] = useState("");
  const [goalStart, setGoalStart] = useState(false);
  const [agentNames, setAgentNames] = useState<Record<string, string>>({});

  // 结果状态
  const [resultItems, setResultItems] = useState<ApplyItem[] | null>(null);
  const [resultDomain, setResultDomain] = useState<{ id: string; name: string } | null>(null);
  const [applyError, setApplyError] = useState("");

  const apply = useApplyProjectTemplate();

  const isScratch = spec?.domain?.type === "scratch";
  const repoCreate = Boolean(spec?.repo?.create);
  // teamAgents 必须稳定——`?? []` 每次渲染都分配新数组，放 effect deps
  // 会造成无限重渲染循环（Maximum update depth exceeded）。useMemo 钉在 spec 上。
  const teamAgents: TplAgent[] = useMemo(() => spec?.team?.agents ?? [], [spec]);

  // 选中模板 / spec 加载完 → 回填默认值（projectName = 模板名；repoSource =
  // repoCreate ? create : existing；rename map 按模板 agent 名播种空串）。
  useEffect(() => {
    const t = templates?.find((x) => x.id === selectedId);
    setProjectName(t?.name ?? "");
    setRepoSource(repoCreate ? "create" : "existing");
    const map: Record<string, string> = {};
    for (const a of teamAgents) if (a.name) map[a.name] = "";
    setAgentNames(map);
    setGitUrl("");
    setGitCredentials("");
    setRepoToken("");
    setGoalStart(false);
  }, [selectedId, repoCreate, teamAgents, templates]);

  const filtered = useMemo(() => {
    if (!templates) return [];
    const q = search.trim().toLowerCase();
    if (!q) return templates;
    return templates.filter((t) =>
      [t.name, t.description, ...(t.tags ?? [])].join(" ").toLowerCase().includes(q)
    );
  }, [templates, search]);

  // renamePayload：仅保留非空且与模板名不同的覆盖项（与 Vue 一致）。
  const renamePayload = useMemo(() => {
    const out: Record<string, string> = {};
    for (const [tpl, override] of Object.entries(agentNames)) {
      const v = override.trim();
      if (v && v !== tpl) out[tpl] = v;
    }
    return out;
  }, [agentNames]);

  const canSubmit =
    !!selectedId &&
    projectName.trim() !== "" &&
    (isScratch || (repoSource === "create" ? repoToken.trim() !== "" : gitUrl.trim() !== ""));

  const step: "select" | "done" = resultItems || applyError || resultDomain ? "done" : "select";

  function reset() {
    setResultItems(null);
    setResultDomain(null);
    setApplyError("");
    setSelectedId("");
    setShowYaml(false);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!canSubmit || apply.isPending) return;
    setApplyError("");
    setResultItems(null);
    setResultDomain(null);
    const body: ApplyProjectOverrides = {
      name: projectName.trim(),
      goal_start: goalStart,
    };
    if (Object.keys(renamePayload).length) body.rename = renamePayload;
    if (!isScratch) {
      if (repoSource === "create") {
        body.repo_token = repoToken.trim();
        body.repo = { create: true };
      } else {
        body.git_url = gitUrl.trim();
        if (gitCredentials.trim()) body.git_credentials = gitCredentials.trim();
      }
    }
    apply.mutate(
      { id: selectedId, body },
      {
        onSuccess: (res) => {
          setResultItems(res.items ?? []);
          if (res.domain) setResultDomain({ id: res.domain.id, name: res.domain.name });
        },
        onError: (err) => setApplyError(String(err)),
      }
    );
  }

  const selectedTemplate = templates?.find((t) => t.id === selectedId);

  return (
    <Dialog
      title="从项目模板创建"
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
            <Button type="submit" form="proj-tpl-form" disabled={!canSubmit || apply.isPending}>
              {apply.isPending ? (
                <span className="flex items-center gap-1.5"><Loader2 className="h-3.5 w-3.5 animate-spin" />创建中…</span>
              ) : (
                "创建项目"
              )}
            </Button>
          </>
        )
      }
    >
      {step === "done" ? (
        <DoneView items={resultItems ?? []} domainName={resultDomain?.name} error={applyError} />
      ) : (
        <div className="space-y-4">
          {/* 模板列表 + 搜索 + 刷新 */}
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
            <Empty icon="📦">暂无可用项目模板 — 点击「刷新」从模板仓库拉取</Empty>
          ) : (
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
              {filtered.map((t) => (
                <TemplateCard
                  key={t.id}
                  t={t}
                  selected={selectedId === t.id}
                  onSelect={() => setSelectedId(t.id)}
                />
              ))}
            </div>
          )}

          {/* 选中模板的预览 + 覆盖表单（spec 未加载完前不渲染输入框，避免空窗） */}
          {selectedId && (
            <form id="proj-tpl-form" onSubmit={handleSubmit} className="space-y-4">
              {detail.isLoading ? (
                <div className="text-sm text-zinc-400 py-4 text-center">加载模板详情…</div>
              ) : spec ? (
                <>
                  {/* YAML 预览 */}
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

                  {/* 将创建摘要 */}
                  <PreviewBox spec={spec} />

                  {/* 覆盖表单 */}
                  <Field label="项目名称" hint="必填，即 domain 名（重名会报 AW.10000010，换名即可）">
                    <input
                      value={projectName}
                      onChange={(e) => setProjectName(e.target.value)}
                      className={inputCls}
                      placeholder="项目/仓库名…"
                      required
                    />
                  </Field>

                  {!isScratch && (
                    <Field label="仓库来源">
                      <div className="flex gap-2">
                        {repoCreate && (
                          <button
                            type="button"
                            onClick={() => setRepoSource("create")}
                            className={cn(
                              "flex-1 px-3 py-2 rounded-lg text-sm border transition",
                              repoSource === "create"
                                ? "border-indigo-400 bg-indigo-50 text-indigo-700"
                                : "border-zinc-200 text-zinc-600 hover:border-zinc-300"
                            )}
                          >
                            新建仓库（GitCode）
                          </button>
                        )}
                        <button
                          type="button"
                          onClick={() => setRepoSource("existing")}
                          className={cn(
                            "flex-1 px-3 py-2 rounded-lg text-sm border transition",
                            repoSource === "existing"
                              ? "border-indigo-400 bg-indigo-50 text-indigo-700"
                              : "border-zinc-200 text-zinc-600 hover:border-zinc-300"
                          )}
                        >
                          使用已有仓库
                        </button>
                      </div>
                    </Field>
                  )}

                  {!isScratch && repoSource === "create" && (
                    <Field label="GitCode 访问令牌" hint="仅本次建仓与仓库读写使用，不持久保存">
                      <input
                        type="password"
                        value={repoToken}
                        onChange={(e) => setRepoToken(e.target.value)}
                        className={inputCls}
                        placeholder="建仓 token…"
                        required
                      />
                    </Field>
                  )}

                  {!isScratch && repoSource === "existing" && (
                    <>
                      <Field label="仓库 Git URL" hint="必填">
                        <input
                          value={gitUrl}
                          onChange={(e) => setGitUrl(e.target.value)}
                          className={inputCls}
                          placeholder="https://gitcode.com/owner/repo.git"
                          required
                        />
                      </Field>
                      <Field label="Git Token" hint="私有仓或需要 push 时必填，将保存为项目平台凭据">
                        <input
                          type="password"
                          value={gitCredentials}
                          onChange={(e) => setGitCredentials(e.target.value)}
                          className={inputCls}
                          placeholder="可选…"
                        />
                      </Field>
                    </>
                  )}

                  {teamAgents.length > 0 && (
                    <fieldset className="border border-zinc-200 rounded-lg p-3 space-y-2">
                      <legend className="text-xs font-medium text-zinc-500 px-1">
                        Agent 命名（留空 = 用模板默认名；重名时按名更新）
                      </legend>
                      {teamAgents.map((a) => (
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

                  {(spec.goals?.length ?? 0) > 0 && (
                    <label className="flex items-center gap-2 text-sm text-zinc-600 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={goalStart}
                        onChange={(e) => setGoalStart(e.target.checked)}
                        className="rounded border-zinc-300"
                      />
                      创建后立即开始执行初始任务（{spec.goals?.length ?? 0} 个）
                    </label>
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

function TemplateCard({
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
        </div>
      </div>
    </button>
  );
}

function PreviewBox({ spec }: { spec: ProjectTemplateSpec }) {
  const repo = spec.repo;
  const repoText = spec.domain?.type === "scratch"
    ? "无仓库项目（scratch），任务产物是项目目录里的文件与汇报。"
    : repo?.create
      ? `在 GitCode 新建${repo.visibility === "public" ? "公开" : "私有"}仓库${repo.auto_init === false ? "" : "（含初始提交）"}并绑定项目。`
      : "使用你提供的已有仓库，绑定项目。";
  const policySuffix = spec.domain?.policy_text
    ? "｜验收要求已随模板预置，创建后可在项目卡片上调整并生成验收规则。"
    : "";
  const team = spec.team;
  const goals = spec.goals ?? [];

  return (
    <div className="border border-zinc-200 rounded-lg p-3 space-y-2 bg-zinc-50/40">
      <div className="flex items-center gap-1.5 text-xs font-medium text-zinc-700">
        <FolderGit2 className="h-3.5 w-3.5" />
        将创建
      </div>
      <p className="text-xs text-zinc-600 leading-relaxed">{repoText}{policySuffix}</p>
      {team?.squad && (
        <p className="text-xs text-zinc-600 flex items-start gap-1.5">
          <Users className="h-3.5 w-3.5 mt-0.5 shrink-0" />
          <span>
            团队：{team.squad.name || "（未命名小队）"}
            （leader {team.squad.leader}
            {team.squad.members?.length ? `，成员 ${team.squad.members.map((m) => m.name).join("、")}` : ""}）
          </span>
        </p>
      )}
      {goals.length > 0 && (
        <p className="text-xs text-zinc-600 flex items-start gap-1.5">
          <ListChecks className="h-3.5 w-3.5 mt-0.5 shrink-0" />
          <span>初始任务：{goals.map((g) => g.title).join("、")}</span>
        </p>
      )}
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
  domainName,
  error,
}: {
  items: ApplyItem[];
  domainName?: string;
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
          <span>项目「{domainName}」创建成功</span>
        ) : (
          <div>
            <p>项目模板应用完成（含失败项）</p>
            <p className="text-xs mt-1 opacity-80">
              失败项可修正后重试：agent/squad 按名 upsert 不会重复创建；项目重名会报错，换名即可。
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
