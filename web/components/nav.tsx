"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Target, Bot, Terminal, Users, Clock, Boxes, Settings, ScrollText, Monitor, Sparkles } from "lucide-react";
import { cn } from "@/lib/utils";

const NAV_ITEMS = [
  { href: "/domains", label: "Project", icon: Boxes },
  { href: "/goals", label: "Goal", icon: Target },
  { href: "/agents", label: "Agent", icon: Bot },
  { href: "/squads", label: "Squad", icon: Users },
  { href: "/runtimes", label: "Runtime", icon: Terminal },
  { href: "/schedules", label: "Schedule", icon: Clock },
  { href: "/machines", label: "Machine", icon: Monitor },
  { href: "/skills", label: "Skills", icon: Sparkles },
  { href: "/logs", label: "Logs", icon: ScrollText },
  { href: "/settings", label: "Settings", icon: Settings },
];

export function Nav() {
  const pathname = usePathname();
  return (
    <nav className="w-56 shrink-0 border-r border-zinc-200/80 bg-white/80 backdrop-blur-sm flex flex-col gap-0.5 p-3">
      <div className="px-3 py-3.5 text-base font-bold tracking-tight bg-gradient-to-r from-indigo-600 to-violet-600 bg-clip-text text-transparent">
        agentwork
      </div>
      {NAV_ITEMS.map((item) => {
        const active = pathname.startsWith(item.href);
        const Icon = item.icon;
        return (
          <Link
            key={item.href}
            href={item.href}
            className={cn(
              "px-3 py-2 rounded-lg text-sm font-medium transition-all duration-150 flex items-center gap-2.5",
              active
                ? "bg-gradient-to-r from-indigo-600 to-violet-600 text-white shadow-sm shadow-indigo-500/25"
                : "text-zinc-600 hover:bg-indigo-50/70 hover:text-indigo-700"
            )}
          >
            <Icon className="h-4 w-4" />
            {item.label}
          </Link>
        );
      })}
    </nav>
  );
}
