"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Target, Bot, Terminal, Users, Clock } from "lucide-react";
import { cn } from "@/lib/utils";

const NAV_ITEMS = [
  { href: "/goals", label: "Goal", icon: Target },
  { href: "/agents", label: "Agent", icon: Bot },
  { href: "/runtimes", label: "Runtime", icon: Terminal },
  { href: "/squads", label: "Squad", icon: Users },
  { href: "/schedules", label: "Schedule", icon: Clock },
];

export function Nav() {
  const pathname = usePathname();
  return (
    <nav className="w-56 shrink-0 border-r border-zinc-200 bg-white flex flex-col gap-0.5 p-3">
      <div className="px-3 py-3 text-sm font-bold text-zinc-900 tracking-tight">
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
              "px-3 py-2 rounded-md text-sm font-medium transition-colors flex items-center gap-2.5",
              active
                ? "bg-zinc-900 text-white"
                : "text-zinc-600 hover:bg-zinc-100 hover:text-zinc-900"
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
