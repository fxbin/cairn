// 排版刻度守卫：字号必须来自刻度令牌，刻度必须由用户偏好的基准字号派生，
// 活跃界面不得再写死数字字号。刻度散落会同时毁掉可读性与用户字号偏好，
// 因此这里用源码级断言把漂移挡在门口。
import { describe, expect, it } from "vitest";

// 守卫需要读取样式表与组件源码。用 Node 内建模块直接读，不为守卫引入
// @types/node 依赖，也不改动共享的 vitest 配置（CSS 处理开启会拖慢全部用例）。
const nodeFs = (
  globalThis as unknown as {
    process?: { getBuiltinModule?: (id: string) => unknown };
  }
).process?.getBuiltinModule?.("node:fs") as
  | { readFileSync: (path: string, encoding: "utf8") => string; readdirSync: (path: string) => string[] }
  | undefined;

if (!nodeFs) {
  throw new Error("排版刻度守卫需要 Node 内建 fs 模块");
}

const fs = nodeFs;
// 目录基准取 vitest 运行根（前端工程根），不依赖 import.meta.url 的形态。
const nodeProcess = (
  globalThis as unknown as { process?: { cwd?: () => string } }
).process;
const projectRoot = nodeProcess?.cwd?.() ?? "";
const componentsDir = `${projectRoot}/src/components/`;
const srcDir = `${projectRoot}/src/`;
const styles = fs.readFileSync(`${srcDir}styles.css`, "utf8");
const appSource = fs.readFileSync(`${srcDir}App.tsx`, "utf8");

/** 刻度档位：标识、次要、正文、标题、展示、等宽代码。 */
const SCALE_TOKENS = ["--fs-label", "--fs-meta", "--fs-body", "--fs-title", "--fs-display", "--fs-code"];

/** 按 App.tsx 的相对导入得出实际接线的组件文件集合。 */
function liveComponentSources(): Record<string, string> {
  const live: Record<string, string> = { "App.tsx": appSource };
  const pattern = /from\s+"\.\/components\/([A-Za-z0-9_-]+)"/g;
  let match = pattern.exec(appSource);
  while (match) {
    const source = fs.readFileSync(`${componentsDir}${match[1]}.tsx`, "utf8");
    live[`components/${match[1]}.tsx`] = source;
    match = pattern.exec(appSource);
  }
  return live;
}

/** 取出内联样式对象里的字号声明；组件属性（fontSize={...}）不在此列。 */
function inlineFontSizes(source: string): string[] {
  const found: string[] = [];
  const stylePattern = /style=\{\{([^}]*)\}\}/g;
  let styleMatch = stylePattern.exec(source);
  while (styleMatch) {
    const sizePattern = /fontSize:\s*([^,}]+)/g;
    let sizeMatch = sizePattern.exec(styleMatch[1]);
    while (sizeMatch) {
      found.push(sizeMatch[1].trim());
      sizeMatch = sizePattern.exec(styleMatch[1]);
    }
    styleMatch = stylePattern.exec(source);
  }
  return found;
}

describe("排版刻度", () => {
  it("样式表里不再出现裸 px 字号", () => {
    expect(styles.match(/font-size:\s*[0-9.]+px/g) ?? []).toEqual([]);
  });

  it("刻度档位齐全且都以用户偏好的基准字号派生", () => {
    for (const token of SCALE_TOKENS) {
      expect(styles).toContain(`${token}:`);
    }
    // 除 --fs-title 直接等于基准外，其余档位都由基准加减派生：
    // 基准变化时整档同比缩放，而不是逐处手改 px。
    const derived = styles.match(/--fs-[a-z]+:\s*([^;]+);/g) ?? [];
    expect(derived.length).toBe(SCALE_TOKENS.length);
    for (const declaration of derived) {
      expect(declaration).toContain("var(--font-size-base)");
    }
  });

  it("基准字号有 CSS 默认值，不依赖脚本先行", () => {
    expect(styles).toMatch(/--font-size-base:\s*14px;/);
  });

  it("每档刻度都被真正使用，不留死令牌", () => {
    for (const token of SCALE_TOKENS) {
      const uses = styles.split(`var(${token})`).length - 1;
      expect(uses, `${token} 未被使用`).toBeGreaterThan(0);
    }
  });

  it("接线中的组件不再写死数字字号", () => {
    const offenders: string[] = [];
    for (const [file, source] of Object.entries(liveComponentSources())) {
      for (const value of inlineFontSizes(source)) {
        if (!value.startsWith('"var(--fs-')) {
          offenders.push(`${file}: fontSize ${value}`);
        }
      }
    }
    expect(offenders).toEqual([]);
  });

  it("守卫覆盖到实际接线的组件面", () => {
    const live = Object.keys(liveComponentSources());
    expect(live).toContain("App.tsx");
    expect(live.length).toBeGreaterThan(1);
  });

  it("用户字号偏好写进基准字号变量", () => {
    expect(appSource).toContain('"--font-size-base"');
  });
});
