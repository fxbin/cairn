/**
 * Provider 连接配置、models.dev 级联目录选择、预设管理与三指纹绑定确认准入面板。
 * 采用顶部已保存预设管理表、物理弹簧折叠双栏级联选择器与三阶门禁配置卡片；
 * 连接生成、探测回执和准入授权保持三阶严格分离，绝不接受或保存明文密钥。
 * Author: fxbin
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type {
  ModelCatalogEntryView,
  ModelCatalogProviderView,
  ProviderPresetView,
  ProviderProbeView,
  ProviderSetupRequest,
} from "../desktop/types";

/**
 * 描述工作区已保存 Provider 预设的兼容视图；支持单测缺省字段安全回退。
 */
type PresetRowItem = Partial<ProviderPresetView> & {
  name: string;
  model: string;
  active: boolean;
};

/**
 * 描述连接配置生成后的落盘路径与模型目录匹配依据。
 */
interface ProviderSetupEvidence {
  config_path: string;
  catalog_matched: boolean;
  notes: string[];
}

/**
 * 描述已知 OpenAI-Compatible 供应商的兼容端点与标准环境变量名补充定义；不含任何密钥值。
 */
interface KnownCompatibleEndpoint {
  baseURL: string;
  keyEnv: string;
  aliases: readonly string[];
}

/**
 * 内置已知 OpenAI-Compatible 网关端点与环境变量名补充表。
 * 当 models.dev 条目未提供 api 字段时，用于预填建议端点并支持中英文别名检索。
 */
const KNOWN_COMPATIBLE_ENDPOINTS: Readonly<Record<string, KnownCompatibleEndpoint>> = {
  deepseek: {
    baseURL: "https://api.deepseek.com/v1",
    keyEnv: "DEEPSEEK_API_KEY",
    aliases: ["deepseek", "深度求索"],
  },
  zhipuai: {
    baseURL: "https://open.bigmodel.cn/api/paas/v4",
    keyEnv: "ZHIPU_API_KEY",
    aliases: ["zhipu", "zai", "bigmodel", "glm", "智谱"],
  },
  zai: {
    baseURL: "https://open.bigmodel.cn/api/paas/v4",
    keyEnv: "ZHIPU_API_KEY",
    aliases: ["zhipu", "zhipuai", "bigmodel", "glm", "智谱"],
  },
  "zhipuai-coding-plan": {
    baseURL: "https://open.bigmodel.cn/api/coding/paas/v4",
    keyEnv: "ZHIPU_API_KEY",
    aliases: ["zhipu", "glm", "智谱"],
  },
  alibaba: {
    baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
    keyEnv: "DASHSCOPE_API_KEY",
    aliases: ["dashscope", "qwen", "bailian", "阿里", "百炼", "通义"],
  },
  "alibaba-cn": {
    baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
    keyEnv: "DASHSCOPE_API_KEY",
    aliases: ["dashscope", "qwen", "bailian", "阿里", "百炼", "通义"],
  },
  moonshotai: {
    baseURL: "https://api.moonshot.cn/v1",
    keyEnv: "MOONSHOT_API_KEY",
    aliases: ["moonshot", "kimi", "月之暗面"],
  },
  "moonshotai-cn": {
    baseURL: "https://api.moonshot.cn/v1",
    keyEnv: "MOONSHOT_API_KEY",
    aliases: ["moonshot", "kimi", "月之暗面"],
  },
  minimax: {
    baseURL: "https://api.minimax.chat/v1",
    keyEnv: "MINIMAX_API_KEY",
    aliases: ["minimax", "海螺"],
  },
  "minimax-cn": {
    baseURL: "https://api.minimax.chat/v1",
    keyEnv: "MINIMAX_API_KEY",
    aliases: ["minimax", "海螺"],
  },
  openrouter: {
    baseURL: "https://openrouter.ai/api/v1",
    keyEnv: "OPENROUTER_API_KEY",
    aliases: ["openrouter"],
  },
  siliconflow: {
    baseURL: "https://api.siliconflow.cn/v1",
    keyEnv: "SILICONFLOW_API_KEY",
    aliases: ["siliconflow", "硅基", "硅基流动"],
  },
  "siliconflow-cn": {
    baseURL: "https://api.siliconflow.cn/v1",
    keyEnv: "SILICONFLOW_API_KEY",
    aliases: ["siliconflow", "硅基", "硅基流动"],
  },
  xiaomi: {
    baseURL: "https://api.xiaomimimo.com/v1",
    keyEnv: "XIAOMI_API_KEY",
    aliases: ["xiaomi", "mimo", "小米"],
  },
  openai: {
    baseURL: "https://api.openai.com/v1",
    keyEnv: "OPENAI_API_KEY",
    aliases: ["openai", "gpt"],
  },
  ollama: {
    baseURL: "http://127.0.0.1:11434/v1",
    keyEnv: "OLLAMA_API_KEY",
    aliases: ["ollama", "local", "本地"],
  },
  xai: {
    baseURL: "https://api.x.ai/v1",
    keyEnv: "XAI_API_KEY",
    aliases: ["xai", "grok"],
  },
  groq: {
    baseURL: "https://api.groq.com/openai/v1",
    keyEnv: "GROQ_API_KEY",
    aliases: ["groq"],
  },
  mistral: {
    baseURL: "https://api.mistral.ai/v1",
    keyEnv: "MISTRAL_API_KEY",
    aliases: ["mistral", "codestral"],
  },
  togetherai: {
    baseURL: "https://api.together.xyz/v1",
    keyEnv: "TOGETHER_API_KEY",
    aliases: ["together"],
  },
  "fireworks-ai": {
    baseURL: "https://api.fireworks.ai/inference/v1",
    keyEnv: "FIREWORKS_API_KEY",
    aliases: ["fireworks"],
  },
  volcengine: {
    baseURL: "https://ark.cn-beijing.volces.com/api/v3",
    keyEnv: "ARK_API_KEY",
    aliases: ["doubao", "ark", "火山", "豆包"],
  },
};

/**
 * 常用编码供应商在级联选择器左栏的置顶优先顺序。
 */
const PRIORITY_PROVIDER_IDS: readonly string[] = [
  "deepseek",
  "zhipuai",
  "zai",
  "zhipuai-coding-plan",
  "alibaba-cn",
  "alibaba",
  "moonshotai-cn",
  "moonshotai",
  "minimax-cn",
  "minimax",
  "openrouter",
  "siliconflow",
  "siliconflow-cn",
  "xiaomi",
  "openai",
  "ollama",
  "anthropic",
  "google",
  "xai",
  "groq",
  "mistral",
];

const PRIORITY_RANK_MAP: ReadonlyMap<string, number> = new Map(
  PRIORITY_PROVIDER_IDS.map((id, index) => [id, index]),
);

/**
 * 离线与单测回退使用的内置精选供应商摘要列表；仅含网关地址与大写环境变量名建议。
 */
const BUILTIN_CATALOG_PROVIDERS: readonly ModelCatalogProviderView[] = [
  {
    id: "deepseek",
    name: "DeepSeek",
    api_base_url: "https://api.deepseek.com/v1",
    env_names: ["DEEPSEEK_API_KEY"],
    model_count: 2,
  },
  {
    id: "zhipuai",
    name: "智谱 BigModel",
    api_base_url: "https://open.bigmodel.cn/api/paas/v4",
    env_names: ["ZHIPU_API_KEY"],
    model_count: 2,
  },
  {
    id: "alibaba-cn",
    name: "阿里云百炼",
    api_base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1",
    env_names: ["DASHSCOPE_API_KEY"],
    model_count: 2,
  },
  {
    id: "moonshotai-cn",
    name: "Kimi (Moonshot)",
    api_base_url: "https://api.moonshot.cn/v1",
    env_names: ["MOONSHOT_API_KEY"],
    model_count: 2,
  },
  {
    id: "minimax-cn",
    name: "MiniMax",
    api_base_url: "https://api.minimax.chat/v1",
    env_names: ["MINIMAX_API_KEY"],
    model_count: 1,
  },
  {
    id: "openrouter",
    name: "OpenRouter",
    api_base_url: "https://openrouter.ai/api/v1",
    env_names: ["OPENROUTER_API_KEY"],
    model_count: 2,
  },
  {
    id: "siliconflow",
    name: "硅基流动 SiliconFlow",
    api_base_url: "https://api.siliconflow.cn/v1",
    env_names: ["SILICONFLOW_API_KEY"],
    model_count: 2,
  },
  {
    id: "xiaomi",
    name: "Xiaomi MiMo",
    api_base_url: "https://api.xiaomimimo.com/v1",
    env_names: ["XIAOMI_API_KEY"],
    model_count: 1,
  },
  {
    id: "openai",
    name: "OpenAI",
    api_base_url: "https://api.openai.com/v1",
    env_names: ["OPENAI_API_KEY"],
    model_count: 2,
  },
  {
    id: "ollama",
    name: "Ollama 本地",
    api_base_url: "http://127.0.0.1:11434/v1",
    env_names: ["OLLAMA_API_KEY"],
    model_count: 1,
  },
];

/**
 * 离线与单测回退使用的内置精选 OpenAI-Compatible 模型列表。
 */
const BUILTIN_CATALOG_MODELS: readonly ModelCatalogEntryView[] = [
  {
    provider: "deepseek",
    id: "deepseek-chat",
    name: "DeepSeek V3",
    family: "deepseek-v3",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 65536,
    output_tokens: 8192,
    input_usd_per_million: 0.27,
    output_usd_per_million: 1.1,
  },
  {
    provider: "deepseek",
    id: "deepseek-reasoner",
    name: "DeepSeek R1",
    family: "deepseek-r1",
    tool_call: true,
    reasoning: true,
    temperature: false,
    context_tokens: 65536,
    output_tokens: 32768,
    input_usd_per_million: 0.55,
    output_usd_per_million: 2.19,
  },
  {
    provider: "zhipuai",
    id: "glm-4.5",
    name: "GLM-4.5",
    family: "glm-4.5",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.6,
    output_usd_per_million: 2.2,
  },
  {
    provider: "zhipuai",
    id: "glm-4.5-air",
    name: "GLM-4.5-Air",
    family: "glm-4.5",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.2,
    output_usd_per_million: 1.1,
  },
  {
    provider: "alibaba-cn",
    id: "qwen3-coder-plus",
    name: "Qwen3 Coder Plus",
    family: "qwen3-coder",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.5,
    output_usd_per_million: 2.0,
  },
  {
    provider: "alibaba-cn",
    id: "qwen-plus",
    name: "Qwen Plus",
    family: "qwen",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 8192,
    input_usd_per_million: 0.4,
    output_usd_per_million: 1.2,
  },
  {
    provider: "moonshotai-cn",
    id: "kimi-k2-0711-preview",
    name: "Kimi K2",
    family: "kimi-k2",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.6,
    output_usd_per_million: 2.5,
  },
  {
    provider: "moonshotai-cn",
    id: "moonshot-v1-128k",
    name: "Moonshot v1 128K",
    family: "moonshot-v1",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 8192,
    input_usd_per_million: 0.8,
    output_usd_per_million: 0.8,
  },
  {
    provider: "minimax-cn",
    id: "MiniMax-M1",
    name: "MiniMax M1",
    family: "minimax-m1",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 1000000,
    output_tokens: 40000,
    input_usd_per_million: 0.4,
    output_usd_per_million: 2.2,
  },
  {
    provider: "openrouter",
    id: "anthropic/claude-sonnet-4",
    name: "Claude Sonnet 4 (OpenRouter)",
    family: "claude-4",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 200000,
    output_tokens: 64000,
    input_usd_per_million: 3.0,
    output_usd_per_million: 15.0,
  },
  {
    provider: "openrouter",
    id: "deepseek/deepseek-r1",
    name: "DeepSeek R1 (OpenRouter)",
    family: "deepseek-r1",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 163840,
    output_tokens: 32768,
    input_usd_per_million: 0.55,
    output_usd_per_million: 2.19,
  },
  {
    provider: "siliconflow",
    id: "deepseek-ai/DeepSeek-V3",
    name: "DeepSeek V3 (SiliconFlow)",
    family: "deepseek-v3",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 65536,
    output_tokens: 8192,
    input_usd_per_million: 0.27,
    output_usd_per_million: 1.1,
  },
  {
    provider: "siliconflow",
    id: "Qwen/Qwen2.5-Coder-32B-Instruct",
    name: "Qwen2.5 Coder 32B",
    family: "qwen2.5-coder",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 32768,
    output_tokens: 8192,
    input_usd_per_million: 0.18,
    output_usd_per_million: 0.18,
  },
  {
    provider: "xiaomi",
    id: "mimo-v2-flash",
    name: "MiMo-V2-Flash",
    family: "mimo-v2",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.1,
    output_usd_per_million: 0.3,
  },
  {
    provider: "openai",
    id: "gpt-4.1",
    name: "GPT-4.1",
    family: "gpt-4.1",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 1047576,
    output_tokens: 32768,
    input_usd_per_million: 2.0,
    output_usd_per_million: 8.0,
  },
  {
    provider: "openai",
    id: "o4-mini",
    name: "o4-mini",
    family: "o4",
    tool_call: true,
    reasoning: true,
    temperature: false,
    context_tokens: 200000,
    output_tokens: 100000,
    input_usd_per_million: 1.1,
    output_usd_per_million: 4.4,
  },
  {
    provider: "ollama",
    id: "qwen2.5-coder:14b",
    name: "Qwen2.5 Coder 14B (Local)",
    family: "qwen2.5-coder",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 32768,
    output_tokens: 8192,
    input_usd_per_million: 0,
    output_usd_per_million: 0,
  },
];

/**
 * 校验输入是否为合法的大写环境变量标识符名称，拒绝任何疑似明文密钥的字符串。
 */
function isValidEnvVarName(name: string): boolean {
  return /^[A-Z_][A-Z0-9_]*$/.test(name);
}

/**
 * 判断非空输入是否违反了环境变量命名约束，用于即时给出防明文泄漏提示。
 */
function isInvalidNonEmptyEnvInput(name: string): boolean {
  return name.trim().length > 0 && !isValidEnvVarName(name);
}

/**
 * 将常用编码供应商置顶排列，其余供应商保持目录原有顺序。
 */
function sortCatalogProviders(
  providers: readonly ModelCatalogProviderView[],
): ModelCatalogProviderView[] {
  return [...providers].sort((a, b) => {
    const rankA = PRIORITY_RANK_MAP.get(a.id.toLowerCase());
    const rankB = PRIORITY_RANK_MAP.get(b.id.toLowerCase());
    if (rankA !== undefined && rankB !== undefined) {
      return rankA - rankB;
    }
    if (rankA !== undefined) return -1;
    if (rankB !== undefined) return 1;
    return 0;
  });
}

/**
 * 判断供应商是否匹配搜索词（支持供应商 ID、名称及内置中英文别名）。
 */
function matchesProviderQuery(provider: ModelCatalogProviderView, lowerQuery: string): boolean {
  if (!lowerQuery) return true;
  if (
    provider.id.toLowerCase().includes(lowerQuery) ||
    provider.name.toLowerCase().includes(lowerQuery)
  ) {
    return true;
  }
  const known = KNOWN_COMPATIBLE_ENDPOINTS[provider.id.toLowerCase()];
  if (known) {
    return known.aliases.some((alias) => alias.toLowerCase().includes(lowerQuery));
  }
  return false;
}

/**
 * 判断模型条目是否匹配检索关键字（匹配模型 ID、展示名称、家族或供应商标识）。
 */
function matchesModelQuery(entry: ModelCatalogEntryView, lowerQuery: string): boolean {
  if (!lowerQuery) return true;
  return (
    entry.id.toLowerCase().includes(lowerQuery) ||
    entry.name.toLowerCase().includes(lowerQuery) ||
    entry.provider.toLowerCase().includes(lowerQuery) ||
    Boolean(entry.family && entry.family.toLowerCase().includes(lowerQuery))
  );
}

/**
 * 在内置精选目录中按供应商、搜索词与 Tool Call 要求过滤模型。
 */
function filterBuiltinModels(
  providerId: string,
  query: string,
  toolCallOnly: boolean,
): ModelCatalogEntryView[] {
  const lowerQuery = query.trim().toLowerCase();
  return BUILTIN_CATALOG_MODELS.filter((entry) => {
    if (providerId && providerId !== "custom" && entry.provider !== providerId) {
      return false;
    }
    if (toolCallOnly && !entry.tool_call) {
      return false;
    }
    if (!lowerQuery) {
      return true;
    }
    return matchesModelQuery(entry, lowerQuery);
  });
}

/**
 * 解析供应商对应的预填 Base URL、合法环境变量名以及端点来源说明。
 */
function resolveProviderPrefill(
  provider: ModelCatalogProviderView,
  source: "models.dev" | "builtin",
): { baseURL: string; keyEnv: string; hint: string } {
  const known = KNOWN_COMPATIBLE_ENDPOINTS[provider.id.toLowerCase()];
  const catalogURL = (provider.api_base_url || "").trim();

  let baseURL = "";
  let hint = "";
  if (catalogURL) {
    baseURL = catalogURL;
    hint =
      source === "models.dev"
        ? "已从 models.dev 目录预填端点，可按实际兼容网关调整"
        : "预填建议端点，可按实际网关调整";
  } else if (known?.baseURL) {
    baseURL = known.baseURL;
    hint = "预填建议端点，可按实际网关调整";
  } else {
    hint = "该供应商未声明默认兼容端点，请手动填写实际 OpenAI-Compatible 网关地址";
  }

  let keyEnv = "";
  for (const candidate of provider.env_names ?? []) {
    const trimmed = candidate.trim();
    if (isValidEnvVarName(trimmed)) {
      keyEnv = trimmed;
      break;
    }
  }
  if (!keyEnv && known?.keyEnv && isValidEnvVarName(known.keyEnv)) {
    keyEnv = known.keyEnv;
  }

  return { baseURL, keyEnv, hint };
}

/**
 * 将上下文窗口 Token 数量格式化为紧凑可读标识（如 128K、1M）。
 */
function formatContextWindow(tokens: number): string {
  if (tokens <= 0) return "";
  if (tokens >= 1_000_000) {
    const millions = tokens % 1_048_576 === 0 ? tokens / 1_048_576 : tokens / 1_000_000;
    const formatted = Number.isInteger(millions) ? String(millions) : millions.toFixed(1);
    return `${formatted.replace(/\.0$/, "")}M`;
  }
  if (tokens >= 1024 && tokens % 1024 === 0) {
    return `${tokens / 1024}K`;
  }
  if (tokens >= 1000) {
    return `${Math.round(tokens / 1000)}K`;
  }
  return String(tokens);
}

/**
 * 格式化每百万 Token 参考美元单价，去除多余尾随零。
 */
function formatUsdPerMillion(value: number): string {
  if (value <= 0) return "0";
  if (value >= 10 || Number.isInteger(value)) {
    return value.toFixed(2).replace(/\.00$/, "");
  }
  return value.toFixed(2).replace(/0$/, "").replace(/\.$/, "");
}

/**
 * 渲染带图标弹簧缩放旋转与里程表滚轮文字翻转的一键复制微交互按钮。
 * 仅用于复制配置文件路径与非敏感哈希指纹，不接触任何密钥文本。
 */
function MorphCopyButton({
  value,
  ariaLabel,
}: {
  value: string;
  ariaLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
      }
    };
  }, []);

  const handleCopy = async () => {
    if (!value) return;
    try {
      if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(value);
      }
    } catch {
      // 剪贴板不可用时静默降级，不阻塞准入核对流程。
    }
    setCopied(true);
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current);
    }
    timerRef.current = window.setTimeout(() => {
      setCopied(false);
      timerRef.current = null;
    }, 1600);
  };

  return (
    <button
      type="button"
      className={"wb-copy-btn" + (copied ? " is-copied" : "")}
      aria-label={ariaLabel}
      onClick={() => void handleCopy()}
    >
      <span className="wb-copy-icon-wrap" aria-hidden="true">
        <svg
          className="wb-icon-copy"
          width="12"
          height="12"
          viewBox="0 0 16 16"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <rect x="5.5" y="5.5" width="8" height="8" rx="1.8" />
          <path d="M10.5 5.5V3.8A1.3 1.3 0 0 0 9.2 2.5H3.8A1.3 1.3 0 0 0 2.5 3.8v5.4a1.3 1.3 0 0 0 1.3 1.3h1.7" />
        </svg>
        <svg
          className="wb-icon-check"
          width="12"
          height="12"
          viewBox="0 0 16 16"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.9"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M3.2 8.4l3.1 3.1 6.5-6.8" />
        </svg>
      </span>
      <span className="wb-copy-roller" aria-hidden="true">
        <span className={"wb-copy-label " + (copied ? "is-exit-up" : "is-enter-center")}>复制</span>
        <span className={"wb-copy-label " + (copied ? "is-enter-center" : "is-enter-down")}>已复制</span>
      </span>
    </button>
  );
}

/**
 * 渲染已保存模型预设管理卡、models.dev 级联目录选择器与三指纹确认门禁的 Provider 设置视图。
 */
export default function ProviderSettings({
  workspace,
  notify,
}: {
  workspace: string;
  notify: (message: string) => void;
}) {
  const [presets, setPresets] = useState<PresetRowItem[]>([]);
  const [confirmDeleteName, setConfirmDeleteName] = useState<string | null>(null);

  const [catalogExpanded, setCatalogExpanded] = useState(true);
  const [catalogSource, setCatalogSource] = useState<"models.dev" | "builtin">("builtin");
  const [catalogProviders, setCatalogProviders] = useState<ModelCatalogProviderView[]>(() =>
    sortCatalogProviders(BUILTIN_CATALOG_PROVIDERS),
  );
  const [catalogModels, setCatalogModels] = useState<ModelCatalogEntryView[]>(() => [
    ...BUILTIN_CATALOG_MODELS,
  ]);
  const [totalProviders, setTotalProviders] = useState<number>(BUILTIN_CATALOG_PROVIDERS.length);
  const [totalModels, setTotalModels] = useState<number>(BUILTIN_CATALOG_MODELS.length);
  const [catalogFetchedAt, setCatalogFetchedAt] = useState<string>("");
  const [catalogStale, setCatalogStale] = useState<boolean>(false);
  const [catalogLoading, setCatalogLoading] = useState<boolean>(false);
  const [catalogSyncing, setCatalogSyncing] = useState<boolean>(false);

  const [selectedProviderId, setSelectedProviderId] = useState<string>("custom");
  const [catalogQuery, setCatalogQuery] = useState<string>("");
  const [toolCallOnly, setToolCallOnly] = useState<boolean>(true);
  const [endpointHint, setEndpointHint] = useState<string>("");

  const [model, setModel] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [keyEnv, setKeyEnv] = useState("");
  const [presetName, setPresetName] = useState("");
  const [userScope, setUserScope] = useState(false);
  const [overwriteConfig, setOverwriteConfig] = useState(false);

  const [reason, setReason] = useState("");
  const [probe, setProbe] = useState<ProviderProbeView | null>(null);
  const [setupEvidence, setSetupEvidence] = useState<ProviderSetupEvidence | null>(null);
  const [busy, setBusy] = useState(false);
  const [revision, setRevision] = useState(0);

  const workspaceGeneration = useRef(0);
  const catalogReqSeqRef = useRef(0);
  const queryTimerRef = useRef<number | null>(null);

  const hasCatalogBridge = typeof desktop.listModelCatalog === "function";
  const hasSavePresetBridge = typeof desktop.saveProviderPreset === "function";
  const hasRemovePresetBridge = typeof desktop.removeProviderPreset === "function";

  useEffect(() => {
    return () => {
      if (queryTimerRef.current !== null) {
        window.clearTimeout(queryTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    workspaceGeneration.current++;
    catalogReqSeqRef.current++;
    setProbe(null);
    setSetupEvidence(null);
    setReason("");
    setPresets([]);
    setConfirmDeleteName(null);
    setBusy(false);
  }, [workspace]);

  useEffect(() => {
    if (typeof desktop.listProviderPresets !== "function") {
      return;
    }
    let alive = true;
    const generation = workspaceGeneration.current;
    void desktop
      .listProviderPresets({ workspace })
      .then((items) => {
        if (alive && generation === workspaceGeneration.current) {
          setPresets(Array.isArray(items) ? items : []);
        }
      })
      .catch((e) => {
        if (alive && generation === workspaceGeneration.current) {
          notify(e instanceof Error ? e.message : String(e));
        }
      });
    return () => {
      alive = false;
    };
  }, [workspace, revision, notify]);

  useEffect(() => {
    if (typeof desktop.listModelCatalog !== "function") {
      return;
    }
    let alive = true;
    const wsGen = workspaceGeneration.current;
    const reqSeq = ++catalogReqSeqRef.current;
    setCatalogLoading(true);
    void desktop
      .listModelCatalog({ tool_call_only: true, limit: 250 })
      .then((view) => {
        if (!alive || wsGen !== workspaceGeneration.current || reqSeq !== catalogReqSeqRef.current) {
          return;
        }
        const sortedProviders = sortCatalogProviders(view.providers ?? []);
        if (sortedProviders.length > 0) {
          setCatalogProviders(sortedProviders);
          setTotalProviders(view.total_providers || sortedProviders.length);
          setCatalogModels(view.models ?? []);
          setTotalModels(view.total_models || (view.models ?? []).length);
          setCatalogFetchedAt(view.fetched_at || "");
          setCatalogStale(Boolean(view.from_stale_cache));
          setCatalogSource("models.dev");
        }
      })
      .catch(() => {
        // 离线或桥接不可达时静默保留内置精选目录，绝不弹窗阻断配置界面。
      })
      .finally(() => {
        if (alive && wsGen === workspaceGeneration.current && reqSeq === catalogReqSeqRef.current) {
          setCatalogLoading(false);
        }
      });
    return () => {
      alive = false;
    };
  }, [workspace]);

  const fetchProviderCatalog = (
    providerId: string,
    nextToolCallOnly: boolean,
    nextQuery: string,
  ) => {
    if (typeof desktop.listModelCatalog !== "function") {
      const fallback = filterBuiltinModels(providerId, nextQuery, nextToolCallOnly);
      setCatalogModels(fallback);
      setTotalModels(fallback.length);
      setCatalogSource("builtin");
      return;
    }
    const wsGen = workspaceGeneration.current;
    const reqSeq = ++catalogReqSeqRef.current;
    setCatalogLoading(true);
    const request =
      providerId !== "custom"
        ? {
            provider: providerId,
            tool_call_only: nextToolCallOnly,
            limit: 300,
          }
        : {
            ...(nextQuery.trim() ? { query: nextQuery.trim() } : {}),
            tool_call_only: nextToolCallOnly,
            limit: 250,
          };

    void desktop
      .listModelCatalog(request)
      .then((view) => {
        if (wsGen !== workspaceGeneration.current || reqSeq !== catalogReqSeqRef.current) {
          return;
        }
        if (Array.isArray(view.providers) && view.providers.length > 0) {
          const sorted = sortCatalogProviders(view.providers);
          setCatalogProviders(sorted);
          setTotalProviders(view.total_providers || sorted.length);
        }
        setCatalogModels(view.models ?? []);
        if (providerId === "custom" && !nextQuery.trim()) {
          setTotalModels(view.total_models || (view.models ?? []).length);
        }
        setCatalogFetchedAt(view.fetched_at || "");
        setCatalogStale(Boolean(view.from_stale_cache));
        setCatalogSource("models.dev");
      })
      .catch(() => {
        if (wsGen !== workspaceGeneration.current || reqSeq !== catalogReqSeqRef.current) {
          return;
        }
        const fallback = filterBuiltinModels(providerId, nextQuery, nextToolCallOnly);
        setCatalogModels(fallback);
        setCatalogSource("builtin");
      })
      .finally(() => {
        if (wsGen === workspaceGeneration.current && reqSeq === catalogReqSeqRef.current) {
          setCatalogLoading(false);
        }
      });
  };

  const handleSyncCatalog = async () => {
    if (catalogSyncing) return;
    if (typeof desktop.listModelCatalog !== "function") {
      setCatalogProviders(sortCatalogProviders(BUILTIN_CATALOG_PROVIDERS));
      setCatalogModels(filterBuiltinModels(selectedProviderId, catalogQuery, toolCallOnly));
      setCatalogSource("builtin");
      return;
    }
    const wsGen = workspaceGeneration.current;
    const reqSeq = ++catalogReqSeqRef.current;
    setCatalogSyncing(true);
    setCatalogLoading(true);
    try {
      const view = await desktop.listModelCatalog({
        refresh: true,
        tool_call_only: toolCallOnly,
        limit: 250,
      });
      if (wsGen !== workspaceGeneration.current || reqSeq !== catalogReqSeqRef.current) {
        return;
      }
      const sorted = sortCatalogProviders(view.providers ?? []);
      if (sorted.length > 0) {
        setCatalogProviders(sorted);
        setTotalProviders(view.total_providers || sorted.length);
      }
      setTotalModels(view.total_models || (view.models ?? []).length);
      setCatalogFetchedAt(view.fetched_at || "");
      setCatalogStale(Boolean(view.from_stale_cache));
      setCatalogSource("models.dev");

      if (selectedProviderId !== "custom") {
        const subSeq = ++catalogReqSeqRef.current;
        const scoped = await desktop.listModelCatalog({
          provider: selectedProviderId,
          tool_call_only: toolCallOnly,
          limit: 300,
        });
        if (wsGen === workspaceGeneration.current && subSeq === catalogReqSeqRef.current) {
          setCatalogModels(scoped.models ?? []);
        }
      } else {
        setCatalogModels(view.models ?? []);
      }
      if (wsGen === workspaceGeneration.current) {
        notify("已同步最新 models.dev 模型目录。");
      }
    } catch {
      if (wsGen === workspaceGeneration.current) {
        setCatalogProviders((prev) =>
          prev.length > 0 ? prev : sortCatalogProviders(BUILTIN_CATALOG_PROVIDERS),
        );
        setCatalogModels((prev) =>
          prev.length > 0
            ? prev
            : filterBuiltinModels(selectedProviderId, catalogQuery, toolCallOnly),
        );
        notify("models.dev 同步暂不可达，已回退保留本地/内置精选目录。");
      }
    } finally {
      if (wsGen === workspaceGeneration.current) {
        setCatalogSyncing(false);
        setCatalogLoading(false);
      }
    }
  };

  const handleSelectCustomProvider = () => {
    setSelectedProviderId("custom");
    setEndpointHint("");
    fetchProviderCatalog("custom", toolCallOnly, catalogQuery);
  };

  const handleSelectProvider = (provider: ModelCatalogProviderView) => {
    setSelectedProviderId(provider.id);
    const prefill = resolveProviderPrefill(provider, catalogSource);
    if (prefill.baseURL) {
      setBaseURL(prefill.baseURL);
    }
    if (prefill.keyEnv) {
      setKeyEnv(prefill.keyEnv);
    }
    setEndpointHint(prefill.hint);
    fetchProviderCatalog(provider.id, toolCallOnly, catalogQuery);
  };

  const handleToggleToolCallOnly = (checked: boolean) => {
    setToolCallOnly(checked);
    fetchProviderCatalog(selectedProviderId, checked, catalogQuery);
  };

  const handleCatalogQueryChange = (nextValue: string) => {
    setCatalogQuery(nextValue);
    if (queryTimerRef.current !== null) {
      window.clearTimeout(queryTimerRef.current);
      queryTimerRef.current = null;
    }
    if (typeof desktop.listModelCatalog !== "function") {
      setCatalogModels(filterBuiltinModels(selectedProviderId, nextValue, toolCallOnly));
      return;
    }
    if (selectedProviderId === "custom") {
      queryTimerRef.current = window.setTimeout(() => {
        queryTimerRef.current = null;
        fetchProviderCatalog("custom", toolCallOnly, nextValue);
      }, 160);
    }
  };

  const handleSelectModel = (entry: ModelCatalogEntryView) => {
    setModel(entry.id);
    if (!baseURL.trim() || !keyEnv.trim()) {
      const matchedProvider = catalogProviders.find((item) => item.id === entry.provider);
      if (matchedProvider) {
        const prefill = resolveProviderPrefill(matchedProvider, catalogSource);
        if (!baseURL.trim() && prefill.baseURL) {
          setBaseURL(prefill.baseURL);
          setEndpointHint(prefill.hint);
        }
        if (!keyEnv.trim() && prefill.keyEnv) {
          setKeyEnv(prefill.keyEnv);
        }
      }
    }
  };

  const loadPresetIntoForm = (item: PresetRowItem) => {
    setBaseURL(item.base_url || "");
    setModel(item.model || "");
    if (item.api_key_env && isValidEnvVarName(item.api_key_env)) {
      setKeyEnv(item.api_key_env);
    } else {
      setKeyEnv(item.api_key_env || "");
    }
    setPresetName(item.name);
    setEndpointHint("");
  };

  const perform = async (action: () => Promise<unknown>, notice: string) => {
    if (busy) return;
    setBusy(true);
    const generation = workspaceGeneration.current;
    try {
      await action();
      if (generation === workspaceGeneration.current) {
        notify(notice);
        setRevision((n) => n + 1);
      }
    } catch (e) {
      if (generation === workspaceGeneration.current) {
        notify(e instanceof Error ? e.message : String(e));
      }
    } finally {
      if (generation === workspaceGeneration.current) {
        setBusy(false);
      }
    }
  };

  const visibleProviders = useMemo(() => {
    const lower = catalogQuery.trim().toLowerCase();
    if (!lower) return catalogProviders;
    const matched = catalogProviders.filter((p) => matchesProviderQuery(p, lower));
    return matched.length > 0 ? matched : catalogProviders;
  }, [catalogProviders, catalogQuery]);

  const displayedModels = useMemo(() => {
    const baseList = catalogModels.filter((entry) => (!toolCallOnly ? true : entry.tool_call));
    const lower = catalogQuery.trim().toLowerCase();
    if (!lower) return baseList;
    const filtered = baseList.filter((entry) => matchesModelQuery(entry, lower));
    if (filtered.length > 0) return filtered;
    // 当搜索词用于定位左侧供应商名称（如“智谱”“百炼”）且点选了该供应商时，保留展示该供应商完整模型列表。
    const activeProvider = catalogProviders.find((p) => p.id === selectedProviderId);
    if (activeProvider && matchesProviderQuery(activeProvider, lower)) {
      return baseList;
    }
    return [];
  }, [catalogModels, catalogProviders, catalogQuery, selectedProviderId, toolCallOnly]);

  const selectedProviderMeta = useMemo(
    () => catalogProviders.find((item) => item.id === selectedProviderId) ?? null,
    [catalogProviders, selectedProviderId],
  );

  const activePreset = presets.find((item) => item.active) ?? null;
  const validEnv = isValidEnvVarName(keyEnv);
  const canAdmit =
    probe?.result === "pass" &&
    !!probe.evidence_fingerprint &&
    !!probe.config_fingerprint &&
    !!probe.request_profile_fingerprint &&
    probe.tool_roundtrip === "ok";

  return (
    <div className="wb-provider-settings" data-testid="provider-settings" style={{ maxWidth: "100%" }}>
      <div className="wb-provider-main">
        {/* 1. 顶部已保存模型预设管理卡 */}
        <section className="wb-settings-section">
          <div
            className="wb-settings-section-title"
            style={{
              justifyContent: "space-between",
              alignItems: "center",
              gap: 10,
              flexWrap: "wrap",
            }}
          >
            <div style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
              <span>已保存模型预设</span>
              <span className="wb-badge is-muted">{presets.length} 项</span>
              {activePreset && (
                <span className="wb-badge is-ok">当前启用：{activePreset.name}</span>
              )}
            </div>
            <div style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
              <button
                type="button"
                disabled={catalogSyncing || busy}
                onClick={() => void handleSyncCatalog()}
                title={
                  hasCatalogBridge
                    ? "从 models.dev 拉取并更新本地缓存目录"
                    : "当前环境使用内置精选目录"
                }
              >
                {catalogSyncing ? "↻ 同步中…" : "↻ 同步 models.dev"}
              </button>
              <button
                type="button"
                aria-expanded={catalogExpanded}
                onClick={() => setCatalogExpanded((open) => !open)}
              >
                {catalogExpanded ? "收起目录选择器" : "展开目录选择器"}
              </button>
            </div>
          </div>

          <div className="wb-settings-card">
            {presets.length === 0 ? (
              <div
                className="wb-settings-card-pad"
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 12,
                  flexWrap: "wrap",
                }}
              >
                <div>
                  <strong style={{ fontSize: 13, color: "var(--wb-title)" }}>
                    当前工作区暂无已保存命名预设
                  </strong>
                  <p style={{ margin: "4px 0 0" }}>
                    可在下方从供应商目录点选模型，填写「同时保存为命名预设」后保存多套网关切换方案；密钥仅引用环境变量名。
                  </p>
                </div>
                <span
                  className={
                    "wb-badge " + (canAdmit ? "is-ok" : probe ? "is-warn" : "is-muted")
                  }
                >
                  {canAdmit
                    ? "探测已通过 · 待确认准入"
                    : probe
                      ? `最近回执：${probe.result}`
                      : "尚未执行连接探测"}
                </span>
              </div>
            ) : (
              <div style={{ overflowX: "auto" }}>
                <table className="wb-preset-table" aria-label="已保存模型预设列表">
                  <thead>
                    <tr>
                      <th>预设名称</th>
                      <th>模型 ID</th>
                      <th>端点 / 协议</th>
                      <th>密钥变量</th>
                      <th>准入模式</th>
                      <th style={{ textAlign: "right" }}>操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {presets.map((item) => {
                      const confirmingDelete = confirmDeleteName === item.name;
                      return (
                        <tr
                          key={item.name}
                          className={"wb-preset-row" + (item.active ? " is-active" : "")}
                        >
                          <td>
                            <div style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
                              <span
                                className={"wb-status-dot " + (item.active ? "is-ok" : "is-muted")}
                                title={item.active ? "当前激活预设" : "未激活预设"}
                                aria-hidden="true"
                              />
                              <strong>{item.name}</strong>
                              {item.active && <span className="wb-badge is-ok">启用中</span>}
                            </div>
                          </td>
                          <td>
                            <code>{item.model || "—"}</code>
                          </td>
                          <td>
                            <div style={{ display: "grid", gap: 2, maxWidth: 240 }}>
                              <span
                                style={{
                                  fontSize: 11.5,
                                  color: "var(--wb-ink)",
                                  overflow: "hidden",
                                  textOverflow: "ellipsis",
                                  whiteSpace: "nowrap",
                                }}
                                title={item.base_url || ""}
                              >
                                {item.base_url || "默认网关"}
                              </span>
                              <small style={{ fontSize: 10.5, color: "var(--wb-muted)" }}>
                                {item.protocol || "openai-chat-completions"}
                              </small>
                            </div>
                          </td>
                          <td>
                            <code>{item.api_key_env || "—"}</code>
                          </td>
                          <td>
                            <span
                              className={
                                "wb-badge " +
                                (item.admission === "optional" ? "is-warn" : "is-accent")
                              }
                            >
                              {item.admission || "required"}
                            </span>
                          </td>
                          <td style={{ textAlign: "right" }}>
                            <div
                              className="wb-actions"
                              style={{ justifyContent: "flex-end", paddingTop: 0 }}
                            >
                              <button
                                type="button"
                                disabled={busy}
                                onClick={() => loadPresetIntoForm(item)}
                              >
                                载入编辑
                              </button>
                              {!item.active && (
                                <button
                                  type="button"
                                  disabled={busy || typeof desktop.useProviderPreset !== "function"}
                                  onClick={() => {
                                    if (typeof desktop.useProviderPreset !== "function") return;
                                    setConfirmDeleteName(null);
                                    void perform(
                                      () => desktop.useProviderPreset(workspace, item.name),
                                      "预设已切换，仅影响后续运行。",
                                    );
                                  }}
                                >
                                  切换启用
                                </button>
                              )}
                              {!item.active && hasRemovePresetBridge && !confirmingDelete && (
                                <button
                                  type="button"
                                  disabled={busy}
                                  onClick={() => setConfirmDeleteName(item.name)}
                                >
                                  删除
                                </button>
                              )}
                              {!item.active && hasRemovePresetBridge && confirmingDelete && (
                                <>
                                  <button
                                    type="button"
                                    disabled={busy}
                                    style={{
                                      borderColor: "var(--wb-danger-line, var(--wb-action))",
                                      color: "var(--wb-danger, var(--wb-action))",
                                    }}
                                    onClick={() => {
                                      if (typeof desktop.removeProviderPreset !== "function") {
                                        return;
                                      }
                                      setConfirmDeleteName(null);
                                      void perform(
                                        () => desktop.removeProviderPreset(workspace, item.name),
                                        `已删除预设「${item.name}」。`,
                                      );
                                    }}
                                  >
                                    确认删除
                                  </button>
                                  <button
                                    type="button"
                                    disabled={busy}
                                    onClick={() => setConfirmDeleteName(null)}
                                  >
                                    取消
                                  </button>
                                </>
                              )}
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </section>

        {/* 2. models.dev 供应商与模型级联选择器（物理弹簧折叠） */}
        <div className="wb-spring-accordion" data-expanded={catalogExpanded ? "true" : "false"}>
          <div className="wb-spring-accordion-inner">
            <section className="wb-settings-section" style={{ paddingBottom: 2 }}>
              <div
                className="wb-settings-section-title"
                style={{ justifyContent: "space-between", flexWrap: "wrap", gap: 8 }}
              >
                <span>供应商与模型级联目录</span>
                <span className="wb-badge is-ok">仅引用环境变量名 · 绝不存明文密钥</span>
              </div>

              <div className="wb-catalog-bar">
                <div>
                  {catalogSource === "models.dev" ? (
                    <>
                      <span className="wb-badge is-ok">
                        models.dev · {totalProviders} 家供应商 · {totalModels} 款模型
                      </span>
                      <span
                        className={"wb-badge " + (catalogStale ? "is-warn" : "is-muted")}
                        title={catalogFetchedAt ? `最近缓存时间：${catalogFetchedAt}` : undefined}
                      >
                        {catalogStale
                          ? "离线缓存副本"
                          : catalogFetchedAt
                            ? "本地缓存已同步"
                            : "实时目录"}
                      </span>
                    </>
                  ) : (
                    <span className="wb-badge is-muted">内置精选目录</span>
                  )}
                  {catalogLoading && <span className="wb-badge is-accent">加载中…</span>}
                </div>

                <input
                  type="search"
                  aria-label="搜索供应商或模型"
                  placeholder="搜索供应商名或模型 ID / 名称（如 deepseek / glm / qwen / claude）…"
                  value={catalogQuery}
                  onChange={(e) => handleCatalogQueryChange(e.target.value)}
                />

                <div>
                  <label className={toolCallOnly ? "current" : ""}>
                    <input
                      type="checkbox"
                      checked={toolCallOnly}
                      onChange={(e) => handleToggleToolCallOnly(e.target.checked)}
                    />
                    仅显示 Tool Call 模型
                  </label>
                </div>
              </div>

              <div className="wb-catalog-picker">
                <div
                  className="wb-catalog-provider-list"
                  role="listbox"
                  aria-label="供应商列表"
                >
                  <button
                    type="button"
                    role="option"
                    className={selectedProviderId === "custom" ? "current" : ""}
                    aria-selected={selectedProviderId === "custom"}
                    onClick={handleSelectCustomProvider}
                  >
                    <span>自定义 OpenAI 兼容</span>
                    <small>{catalogQuery.trim() ? displayedModels.length : "自由"}</small>
                  </button>

                  {visibleProviders.map((provider) => {
                    const selected = selectedProviderId === provider.id;
                    return (
                      <button
                        type="button"
                        role="option"
                        key={provider.id}
                        className={selected ? "current" : ""}
                        aria-selected={selected}
                        onClick={() => handleSelectProvider(provider)}
                      >
                        <span>{provider.name || provider.id}</span>
                        <small>{provider.model_count}</small>
                      </button>
                    );
                  })}
                </div>

                <div className="wb-catalog-model-list" aria-label="模型候选列表">
                  <div
                    style={{
                      display: "flex",
                      alignItems: "center",
                      justifyContent: "space-between",
                      gap: 8,
                      flexWrap: "wrap",
                      padding: "4px 8px 6px",
                      borderBottom: "1px solid var(--wb-line)",
                      fontSize: 11.5,
                      color: "var(--wb-muted)",
                    }}
                  >
                    <span>
                      {selectedProviderMeta
                        ? `${selectedProviderMeta.name}（${displayedModels.length} 款可选，点击即填入模型 ID）`
                        : catalogQuery.trim()
                          ? `检索匹配结果（${displayedModels.length} 款，点击填入模型 ID）`
                          : "精选推荐模型（点击左侧供应商预填网关，点击右侧模型填入模型 ID）"}
                    </span>
                    {model.trim() && (
                      <code style={{ fontSize: 11, color: "var(--wb-action)" }}>
                        当前已选：{model.trim()}
                      </code>
                    )}
                  </div>

                  {displayedModels.length === 0 ? (
                    <div style={{ padding: "24px 14px", textAlign: "center" }}>
                      <p style={{ margin: 0 }}>
                        当前筛选条件下暂无匹配模型。可尝试清除搜索词、关闭 Tool Call 过滤，或直接在下方手动填写模型 ID。
                      </p>
                    </div>
                  ) : (
                    displayedModels.map((entry) => {
                      const isCurrentModel = model.trim() === entry.id;
                      const ctxLabel = formatContextWindow(entry.context_tokens);
                      const hasPrice =
                        entry.input_usd_per_million > 0 || entry.output_usd_per_million > 0;
                      return (
                        <button
                          type="button"
                          key={`${entry.provider}:${entry.id}`}
                          className={"wb-model-option" + (isCurrentModel ? " is-selected" : "")}
                          aria-pressed={isCurrentModel}
                          onClick={() => handleSelectModel(entry)}
                        >
                          <div>
                            <strong>{entry.name || entry.id}</strong>
                            <code>
                              {selectedProviderId === "custom"
                                ? `${entry.provider} · ${entry.id}`
                                : entry.id}
                            </code>
                          </div>
                          <div style={{ justifyContent: "flex-start" }}>
                            {entry.tool_call && (
                              <span className="wb-cap-pill is-tool">Tool Call</span>
                            )}
                            {entry.reasoning && (
                              <span className="wb-cap-pill is-reason">Reasoning</span>
                            )}
                            {ctxLabel && (
                              <span className="wb-cap-pill is-ctx">{ctxLabel} ctx</span>
                            )}
                            {hasPrice && (
                              <span
                                className="wb-cap-pill is-price"
                                title="目录参考单价（每百万输入/输出 Token 美元报价，不代表网关实际账单）"
                              >
                                ${formatUsdPerMillion(entry.input_usd_per_million)}/$
                                {formatUsdPerMillion(entry.output_usd_per_million)}
                              </span>
                            )}
                          </div>
                        </button>
                      );
                    })
                  )}
                </div>
              </div>
            </section>
          </div>
        </div>

        {/* 3. 三段式安全配置流：连接配置、真实探测回执、准入写入与门禁 */}
        <section className="wb-settings-section">
          <div className="wb-settings-section-title">三阶安全门禁与连接配置</div>
          <div className="wb-admission-steps" style={{ margin: "0 0 14px" }}>
            <div className={"wb-admission-card" + (setupEvidence ? " is-ok" : "")}>
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 6,
                }}
              >
                <strong>① 连接配置</strong>
                <span
                  className={
                    "wb-badge " + (setupEvidence ? "is-ok" : validEnv ? "is-accent" : "is-muted")
                  }
                >
                  {setupEvidence ? "已生成" : "环境变量引用"}
                </span>
              </div>
              <p>
                通过 <code>api_key_env</code> 绑定本机环境变量名，绝不落盘密钥明文；模型目录建议不等同于渠道账单。
              </p>
            </div>

            <div className={"wb-admission-card" + (probe?.result === "pass" ? " is-ok" : "")}>
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 6,
                }}
              >
                <strong>② 真实探测回执</strong>
                <span
                  className={
                    "wb-badge " +
                    (probe?.result === "pass" ? "is-ok" : probe ? "is-warn" : "is-muted")
                  }
                >
                  {probe ? probe.result : "待探测"}
                </span>
              </div>
              <p>
                核验工具调用回环（<code>{probe?.tool_roundtrip || "未运行"}</code>）与流式用量报告（
                <code>{probe ? (probe.usage_streamed ? "true" : "false") : "未报告"}</code>）。
              </p>
            </div>

            <div className={"wb-admission-card" + (canAdmit ? " is-accent" : "")}>
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                  gap: 6,
                }}
              >
                <strong>③ 准入写入与门禁</strong>
                <span className={"wb-badge " + (canAdmit ? "is-accent" : "is-muted")}>
                  {canAdmit ? "待人工确认" : "门禁锁定"}
                </span>
              </div>
              <div style={{ display: "grid", gap: 4 }}>
                <span className="wb-hash-pill" title="探测证据指纹">
                  evidence_fingerprint: {probe?.evidence_fingerprint || "未生成"}
                </span>
                <span className="wb-hash-pill" title="工作区配置指纹">
                  config_fingerprint: {probe?.config_fingerprint || "未生成"}
                </span>
                <span className="wb-hash-pill" title="请求画像指纹">
                  request_profile_fingerprint: {probe?.request_profile_fingerprint || "未生成"}
                </span>
              </div>
            </div>
          </div>

          <div className="wb-settings-card">
            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>
                  Base URL <span className="wb-setting-key">provider.base_url</span>
                </strong>
                <p>OpenAI-Compatible 协议服务端地址（通常以 /v1 结尾）。</p>
                {endpointHint && (
                  <p style={{ margin: "3px 0 0", color: "var(--wb-action)", fontSize: 11.5 }}>
                    {endpointHint}
                  </p>
                )}
              </div>
              <input
                aria-label="Provider Base URL"
                placeholder="https://…/v1"
                value={baseURL}
                onChange={(e) => {
                  setBaseURL(e.target.value);
                  if (endpointHint) setEndpointHint("");
                }}
                style={{ flex: "0 1 320px", width: "100%" }}
              />
            </div>

            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>
                  模型 ID <span className="wb-setting-key">provider.model</span>
                </strong>
                <p>请求调用的目标模型标识符；可从上方级联目录一键点选，目录匹配不代表网关真实计费。</p>
              </div>
              <input
                aria-label="Provider 模型 ID"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="模型名称"
                style={{ flex: "0 1 320px", width: "100%" }}
              />
            </div>

            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>
                  API Key 环境变量名称 <span className="wb-setting-key">provider.api_key_env</span>
                </strong>
                <p>
                  这里只填写环境变量名（如 <code>OPENAI_API_KEY</code>），绝不粘贴或保存 API Key 明文。
                </p>
                {isInvalidNonEmptyEnvInput(keyEnv) && (
                  <p style={{ margin: "4px 0 0", color: "var(--wb-danger, var(--wb-action))" }}>
                    环境变量名称仅允许大写字母、数字与下划线（且不能以数字开头）；已拦截疑似明文密钥或非法变量名。
                  </p>
                )}
              </div>
              <input
                aria-label="API Key 环境变量名称"
                value={keyEnv}
                onChange={(e) => setKeyEnv(e.target.value)}
                placeholder="OPENAI_API_KEY"
                style={{ flex: "0 1 320px", width: "100%" }}
              />
            </div>

            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>配置写入范围与覆盖策略</strong>
                <p>
                  默认写入当前工作区 <code>.agent/config.yaml</code> 且不覆盖既有文件；开启覆盖时自动备份旧配置为{" "}
                  <code>config.yaml.bak</code>。
                </p>
              </div>
              <div
                style={{
                  display: "inline-flex",
                  alignItems: "center",
                  gap: 12,
                  flexWrap: "wrap",
                  justifyContent: "flex-end",
                }}
              >
                <div className="wb-segmented" role="group" aria-label="配置写入作用域">
                  <button
                    type="button"
                    className={!userScope ? "current" : ""}
                    aria-pressed={!userScope}
                    onClick={() => setUserScope(false)}
                  >
                    工作区配置
                  </button>
                  <button
                    type="button"
                    className={userScope ? "current" : ""}
                    aria-pressed={userScope}
                    onClick={() => setUserScope(true)}
                  >
                    用户级配置
                  </button>
                </div>
                <div style={{ display: "inline-flex", alignItems: "center", gap: 7 }}>
                  <span style={{ fontSize: 12, color: "var(--wb-muted)" }}>允许覆盖</span>
                  <button
                    type="button"
                    role="switch"
                    aria-label="允许覆盖既有配置"
                    aria-checked={overwriteConfig}
                    className={"wb-toggle-switch" + (overwriteConfig ? " is-checked" : "")}
                    onClick={() => setOverwriteConfig((prev) => !prev)}
                  >
                    <span className="wb-toggle-thumb" />
                  </button>
                </div>
              </div>
            </div>

            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>
                  同时保存为命名预设 <span className="wb-setting-key">provider.presets</span>
                </strong>
                <p>
                  可选填写预设名称（如 <code>deepseek-coder</code>）；随「生成配置」一并落盘，或点击「仅保存为预设」收录到预设表。
                </p>
              </div>
              <input
                aria-label="同时保存为命名预设"
                value={presetName}
                onChange={(e) => setPresetName(e.target.value)}
                placeholder="可选：预设名称（留空不创建预设）"
                style={{ flex: "0 1 320px", width: "100%" }}
              />
            </div>

            <div className="wb-setting-row">
              <div className="wb-setting-meta">
                <strong>生成配置与连接探测</strong>
                <p>模型连接需先完成真实探测再人工准入；切换或生成配置不改变已冻结运行。</p>
              </div>
              <div className="wb-actions" style={{ paddingTop: 0 }}>
                <button
                  type="button"
                  disabled={
                    busy ||
                    !baseURL.trim() ||
                    !model.trim() ||
                    !validEnv ||
                    (!userScope && !workspace)
                  }
                  onClick={() =>
                    void perform(async () => {
                      const generation = workspaceGeneration.current;
                      const trimmedPreset = presetName.trim();
                      const setupRequest: ProviderSetupRequest = {
                        workspace,
                        model: model.trim(),
                        base_url: baseURL.trim(),
                        api_key_env: keyEnv.trim(),
                        overwrite: overwriteConfig,
                        user_scope: userScope,
                        ...(trimmedPreset
                          ? { save_preset: trimmedPreset, overwrite_preset: overwriteConfig }
                          : {}),
                      };
                      const ack = await desktop.runProviderSetup(setupRequest);
                      if (generation === workspaceGeneration.current) {
                        setSetupEvidence({
                          config_path: ack.config_path,
                          catalog_matched: ack.catalog_matched,
                          notes: ack.notes || [],
                        });
                        setProbe(null);
                      }
                    }, "已生成连接配置；仍需独立探测和准入。")
                  }
                >
                  生成配置
                </button>
                {hasSavePresetBridge && (
                  <button
                    type="button"
                    disabled={
                      busy ||
                      !presetName.trim() ||
                      !baseURL.trim() ||
                      !model.trim() ||
                      !validEnv ||
                      (!userScope && !workspace)
                    }
                    onClick={() =>
                      void perform(async () => {
                        if (typeof desktop.saveProviderPreset !== "function") return;
                        await desktop.saveProviderPreset({
                          workspace,
                          name: presetName.trim(),
                          base_url: baseURL.trim(),
                          model: model.trim(),
                          api_key_env: keyEnv.trim(),
                          overwrite: overwriteConfig,
                          user_scope: userScope,
                        });
                      }, `已保存预设「${presetName.trim()}」，可在上方预设表中切换启用。`)
                    }
                  >
                    仅保存为预设
                  </button>
                )}
                <button
                  type="button"
                  className="wb-primary"
                  disabled={busy || !workspace}
                  onClick={() =>
                    void perform(async () => {
                      const generation = workspaceGeneration.current;
                      const result = await desktop.probeProvider({ workspace });
                      if (generation === workspaceGeneration.current) setProbe(result);
                    }, "探测已完成，请根据回执判定是否准入。")
                  }
                >
                  探测当前 Provider
                </button>
              </div>
            </div>
          </div>
        </section>

        {(setupEvidence || probe) && (
          <section className="wb-settings-section">
            <div className="wb-settings-section-title">配置生成依据与真实探测准入</div>

            {setupEvidence && (
              <section
                className="wb-settings-card wb-settings-card-pad wb-provider-probe"
                data-testid="provider-setup-evidence"
                role="status"
                style={{ marginTop: 0 }}
              >
                <div
                  style={{
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 8,
                    flexWrap: "wrap",
                  }}
                >
                  <strong>配置生成依据</strong>
                  <span
                    className={
                      "wb-badge " + (setupEvidence.catalog_matched ? "is-ok" : "is-warn")
                    }
                  >
                    {setupEvidence.catalog_matched ? "目录候选匹配" : "目录未可靠命中"}
                  </span>
                </div>
                <div
                  style={{
                    display: "flex",
                    alignItems: "center",
                    gap: 8,
                    flexWrap: "wrap",
                    marginTop: 8,
                  }}
                >
                  <p style={{ margin: 0 }}>文件：{setupEvidence.config_path}</p>
                  {setupEvidence.config_path && (
                    <MorphCopyButton
                      value={setupEvidence.config_path}
                      ariaLabel="复制配置路径"
                    />
                  )}
                </div>
                <p>
                  {setupEvidence.catalog_matched
                    ? "目录有匹配候选；价格仍需核对 Provider 与计费渠道。"
                    : "目录未可靠命中；不要将生成的价格视为当前网关报价。"}
                </p>
                {setupEvidence.notes.length > 0 && (
                  <ul>
                    {setupEvidence.notes.map((note, index) => (
                      <li key={index}>{note}</li>
                    ))}
                  </ul>
                )}
              </section>
            )}

            {probe && (
              <div
                className="wb-settings-card wb-settings-card-pad wb-provider-probe"
                role="status"
                style={{ marginTop: setupEvidence ? 12 : 0 }}
              >
                <div
                  style={{
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 8,
                    flexWrap: "wrap",
                  }}
                >
                  <strong>探测结果：{probe.result}</strong>
                  <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                    <span
                      className={
                        "wb-badge " + (probe.tool_roundtrip === "ok" ? "is-ok" : "is-warn")
                      }
                    >
                      tool_roundtrip: {probe.tool_roundtrip || "未报告"}
                    </span>
                    <span
                      className={"wb-badge " + (probe.usage_streamed ? "is-ok" : "is-muted")}
                    >
                      usage_streamed: {probe.usage_streamed ? "true" : "false"}
                    </span>
                  </div>
                </div>
                <p>
                  模型：{probe.model || "未知"} · 工具回环：{probe.tool_roundtrip || "未报告"} · 用量流：
                  {probe.usage_streamed ? "已报告" : "未知/未报告"}
                </p>
                {probe.error && (
                  <p style={{ color: "var(--wb-danger, var(--wb-action))" }}>{probe.error}</p>
                )}
                <div style={{ display: "grid", gap: 6, margin: "8px 0" }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <span className="wb-hash-pill">
                      evidence_fingerprint: {probe.evidence_fingerprint || "—"}
                    </span>
                    {probe.evidence_fingerprint && (
                      <MorphCopyButton
                        value={probe.evidence_fingerprint}
                        ariaLabel="复制证据指纹"
                      />
                    )}
                  </div>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <span className="wb-hash-pill">
                      config_fingerprint: {probe.config_fingerprint || "—"}
                    </span>
                    {probe.config_fingerprint && (
                      <MorphCopyButton
                        value={probe.config_fingerprint}
                        ariaLabel="复制配置指纹"
                      />
                    )}
                  </div>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <span className="wb-hash-pill">
                      request_profile_fingerprint: {probe.request_profile_fingerprint || "—"}
                    </span>
                    {probe.request_profile_fingerprint && (
                      <MorphCopyButton
                        value={probe.request_profile_fingerprint}
                        ariaLabel="复制请求画像指纹"
                      />
                    )}
                  </div>
                </div>
                {!canAdmit && <p>当前证据不满足准入确认条件，不提供授权按钮。</p>}
                {canAdmit && (
                  <>
                    <label>
                      准入决定理由
                      <textarea
                        aria-label="准入决定理由"
                        value={reason}
                        onChange={(e) => setReason(e.target.value)}
                        placeholder="说明实测范围和你确认准入的依据…"
                      />
                    </label>
                    <button
                      type="button"
                      className="wb-primary"
                      disabled={busy || !reason.trim()}
                      onClick={() =>
                        void perform(async () => {
                          const generation = workspaceGeneration.current;
                          await desktop.confirmProviderAdmission({
                            workspace,
                            evidence_fingerprint: probe.evidence_fingerprint!,
                            config_fingerprint: probe.config_fingerprint!,
                            request_profile_fingerprint: probe.request_profile_fingerprint!,
                            reason: reason.trim(),
                          });
                          if (generation === workspaceGeneration.current) {
                            setProbe(null);
                            setReason("");
                          }
                        }, "用户确认准入已记录。")
                      }
                    >
                      明确确认准入
                    </button>
                  </>
                )}
              </div>
            )}
          </section>
        )}

        <p className="wb-settings-footnote">
          连接配置写入工作区 <code>.agent/config.yaml</code>（或用户级 <code>~/.cairn/config.yaml</code>
          ）；本机密钥仅从环境变量按需读取（如 <code>~/.cairn/keys.env</code>
          ），绝不进入配置文件、Event Journal 或错误信息。
        </p>
      </div>
    </div>
  );
}
