import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody } from "../desktop/types";

/**
 * 设置面板：Provider/配置来源、权限预算、MCP 与 Skills 真实状态。
 * 配置存在 ≠ 连接成功 ≠ 已授权；此处只读展示并区分。
 */
export function SettingsPanel({
  workspacePath,
  onError,
}: {
  workspacePath: string;
  onError: (error: ErrorBody) => void;
}) {
  const [doctor, setDoctor] = useState("加载中…");
  const [mcp, setMcp] = useState("（未加载）");
  const [skills, setSkills] = useState("（未加载）");
  const [theme, setTheme] = useState<string>(() => localStorage.getItem("cairn.theme") || "dark");

  useEffect(() => {
    if (!workspacePath) {
      setDoctor("未选择工作区");
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const [doctorResult, mcpResult, skillsResult] = await Promise.all([
          desktop.doctor({ workspace: workspacePath }),
          desktop.mcpStatus({ workspace: workspacePath }).catch(() => "MCP 状态不可用"),
          desktop.listSkills({ workspace: workspacePath }).catch(() => "Skills 不可用"),
        ]);
        if (!cancelled) {
          setDoctor(JSON.stringify(doctorResult, null, 2).slice(0, 4000));
          setMcp(JSON.stringify(mcpResult, null, 2).slice(0, 2000));
          setSkills(JSON.stringify(skillsResult, null, 2).slice(0, 2000));
        }
      } catch (error) {
        if (!cancelled) {
          setDoctor("诊断不可用");
          onError(
            error && typeof error === "object" && "body" in error
              ? (error as { body: ErrorBody }).body
              : { code: "internal", message: String(error) },
          );
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [workspacePath]);

  const applyTheme = (value: string) => {
    setTheme(value);
    localStorage.setItem("cairn.theme", value);
    document.documentElement.dataset.theme = value;
  };

  return (
    <div className="scroll-y min-h-0 flex-1 space-y-3 text-xs" data-testid="settings-panel">
      <div>
        <div className="mb-1 font-medium">界面</div>
        <label className="muted mr-2" htmlFor="theme-select">
          主题
        </label>
        <select
          id="theme-select"
          className="input w-32"
          value={theme}
          onChange={(event) => applyTheme(event.target.value)}
        >
          <option value="dark">深色</option>
          <option value="light">浅色</option>
        </select>
      </div>
      <div>
        <div className="mb-1 font-medium">生效配置 / Provider / 预算</div>
        <pre className="whitespace-pre-wrap rounded-md bg-[var(--c-bg)] p-2">{doctor}</pre>
      </div>
      <div>
        <div className="mb-1 font-medium">MCP（声明 / 状态，不等于已授权）</div>
        <pre className="whitespace-pre-wrap rounded-md bg-[var(--c-bg)] p-2">{mcp}</pre>
      </div>
      <div>
        <div className="mb-1 font-medium">Skills（本地内容包，不授予权限）</div>
        <pre className="whitespace-pre-wrap rounded-md bg-[var(--c-bg)] p-2">{skills}</pre>
      </div>
    </div>
  );
}
