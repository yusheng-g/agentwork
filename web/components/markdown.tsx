"use client";

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { cn } from "@/lib/utils";

// Markdown 渲染 + mention:// 链接转 chip：
// agent 的输出（## 标题 / 列表 / 代码块 / **加粗**）终于像样；
// 结构化 mention 链接在 markdown 里同样渲染成高亮 @chip（与评论区一致）。
export function Markdown({
  content,
  agentName,
  className,
}: {
  content: string;
  agentName?: (id: string) => string;
  className?: string;
}) {
  return (
    <div className={cn("text-sm leading-relaxed", className)}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ href, children }) => {
            const m = href?.match(/^mention:\/\/(agent|squad|human|all)\/(.+)$/);
            if (m && children) {
              const id = m[2];
              const label =
                m[1] === "all"
                  ? "all"
                  : m[1] === "agent" && agentName
                    ? agentName(id)
                    : String(children).replace(/^@/, "");
              return (
                <span
                  className="inline-flex items-center px-1.5 py-0.5 rounded bg-purple-50 text-purple-700 ring-1 ring-purple-200 text-xs font-medium mx-0.5"
                  title={`mention ${m[1]}`}
                >
                  @{label}
                </span>
              );
            }
            return (
              <a href={href} target="_blank" rel="noreferrer" className="text-indigo-600 hover:underline">
                {children}
              </a>
            );
          },
          h1: ({ children }) => (
            <h1 className="text-base font-semibold mt-3 mb-1.5 first:mt-0">{children}</h1>
          ),
          h2: ({ children }) => (
            <h2 className="text-[15px] font-semibold mt-3 mb-1 first:mt-0">{children}</h2>
          ),
          h3: ({ children }) => (
            <h3 className="text-sm font-semibold mt-2.5 mb-1 first:mt-0">{children}</h3>
          ),
          p: ({ children }) => <p className="my-1.5">{children}</p>,
          ul: ({ children }) => <ul className="list-disc pl-5 my-1.5 space-y-0.5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal pl-5 my-1.5 space-y-0.5">{children}</ol>,
          li: ({ children }) => <li>{children}</li>,
          code: ({ className, children }) => {
            // Inline code vs block (rendered by pre)。无语言标注的围栏块没有
            // language- class，仅靠 class 判断会把它误判成行内代码——深色
            // pre 里嵌一条浅色 chip。补一个换行内容检测兜底。
            const isBlock =
              className?.includes("language-") ||
              String(children ?? "").includes("\n");
            if (isBlock) {
              return <code className={className}>{children}</code>;
            }
            return (
              <code className="px-1 py-0.5 rounded bg-zinc-100 text-zinc-800 text-[12px] font-mono">
                {children}
              </code>
            );
          },
          pre: ({ children }) => (
            <pre className="my-2 p-3 rounded-lg bg-zinc-900 text-zinc-100 text-[12px] font-mono overflow-x-auto">
              {children}
            </pre>
          ),
          blockquote: ({ children }) => (
            <blockquote className="my-2 pl-3 border-l-2 border-zinc-300 text-zinc-500">{children}</blockquote>
          ),
          table: ({ children }) => (
            <div className="my-2 overflow-x-auto">
              <table className="text-xs border-collapse">{children}</table>
            </div>
          ),
          th: ({ children }) => (
            <th className="border border-zinc-200 px-2 py-1 bg-zinc-50 text-left font-medium">{children}</th>
          ),
          td: ({ children }) => <td className="border border-zinc-200 px-2 py-1">{children}</td>,
          strong: ({ children }) => <strong className="font-semibold">{children}</strong>,
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
}
