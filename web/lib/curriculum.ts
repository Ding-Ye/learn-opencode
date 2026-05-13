// The locked curriculum from the plan. SessionNav and the landing page both
// read from this single source of truth. Slugs match docs/{zh,en}/<slug>.md.
//
// "available: false" means the chapter exists in the curriculum but its
// docs aren't written yet — the link will render but go to a placeholder.

export type ChapterMeta = {
  slug: string;
  num: string; // "s01", "s02", "s_full"
  title: { zh: string; en: string };
  available: boolean;
};

export const CURRICULUM: ChapterMeta[] = [
  {
    slug: "s01-minimum-loop",
    num: "s01",
    title: { zh: "最小 agent loop", en: "Minimum agent loop" },
    available: true,
  },
  {
    slug: "s02-message-parts",
    num: "s02",
    title: { zh: "消息与 Part 模型", en: "Messages and Parts" },
    available: true,
  },
  {
    slug: "s03-tool-registry",
    num: "s03",
    title: { zh: "工具注册表", en: "Tool registry" },
    available: true,
  },
  {
    slug: "s04-permission-eval",
    num: "s04",
    title: { zh: "权限求值", en: "Permission evaluator" },
    available: true,
  },
  {
    slug: "s05-provider-iface",
    num: "s05",
    title: { zh: "Provider 抽象", en: "Provider abstraction" },
    available: true,
  },
  {
    slug: "s06-streaming-loop",
    num: "s06",
    title: { zh: "流式循环", en: "Streaming loop" },
    available: true,
  },
  {
    slug: "s07-session-store",
    num: "s07",
    title: { zh: "会话存储", en: "Session storage" },
    available: true,
  },
  {
    slug: "s08-config-load",
    num: "s08",
    title: { zh: "配置加载", en: "Config loading" },
    available: false,
  },
  {
    slug: "s09-agent-registry",
    num: "s09",
    title: { zh: "Agent 注册表", en: "Agent registry" },
    available: false,
  },
  {
    slug: "s10-tool-loop",
    num: "s10",
    title: { zh: "工具执行循环", en: "Tool execution loop" },
    available: false,
  },
  {
    slug: "s11-skills",
    num: "s11",
    title: { zh: "技能发现", en: "Skill discovery" },
    available: false,
  },
  {
    slug: "s12-mcp-client",
    num: "s12",
    title: { zh: "MCP 客户端", en: "MCP client" },
    available: false,
  },
  {
    slug: "s13-lsp-client",
    num: "s13",
    title: { zh: "LSP 客户端", en: "LSP client" },
    available: false,
  },
  {
    slug: "s14-cost-and-recovery",
    num: "s14",
    title: { zh: "成本与错误恢复", en: "Cost & error recovery" },
    available: false,
  },
  {
    slug: "s_full-integration",
    num: "s_full",
    title: { zh: "端到端集成", en: "End-to-end integration" },
    available: false,
  },
  {
    slug: "appendix-a-provider-philosophy",
    num: "A",
    title: {
      zh: "附录 A · Provider 抽象哲学",
      en: "Appendix A · Provider abstraction philosophy",
    },
    available: false,
  },
  {
    slug: "appendix-b-upstream-map",
    num: "B",
    title: {
      zh: "附录 B · 上游源码导读地图",
      en: "Appendix B · Upstream source-reading map",
    },
    available: false,
  },
];

export type Locale = "zh" | "en";

export function chapterTitle(c: ChapterMeta, locale: Locale): string {
  return c.title[locale];
}
