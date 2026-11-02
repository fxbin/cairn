/**
 * Desktop 2.0 design contracts: the new Wails entry loads only the v2 theme.
 * Legacy typography guards were coupled to a stylesheet no longer referenced by the app.
 */
import { describe, expect, it } from "vitest";
const fs = (globalThis as unknown as {process?: {getBuiltinModule?: (s:string)=>unknown}}).process?.getBuiltinModule?.("node:fs") as
  {readFileSync:(p:string,enc:"utf8")=>string}|undefined;
const cwd=(globalThis as unknown as {process?: {cwd?:()=>string}}).process?.cwd?.();
if(!fs||!cwd)throw new Error("需要 Node 内建 fs 与工作目录");
const read=(path:string)=>fs.readFileSync(cwd+"/"+path,"utf8");
const css=read("src/v2/tokens.css");
const entry=read("src/main.tsx");
const app=read("src/v2/AppV2.tsx");
describe("Desktop 2.0 theme migration",()=>{
  it("only loads the new application and its own theme",()=>{
    expect(entry).toContain('from "./v2/AppV2"');
    expect(entry).toContain('import "./v2/tokens.css"');
    expect(entry).not.toContain('import "./styles.css"');
    expect(entry).not.toContain('from "./App"');
  });
  it("defines the frozen Cairn brand colors",()=>{
    for(const token of ["--v2-shell","--v2-side","--v2-paper","--v2-ink","--v2-action","--v2-font-size-base"])expect(css).toContain(token);
    expect(css).toContain("#b55c35");
  });
  it("supports visible focus, reduced motion and narrow windows",()=>{
    expect(css).toContain(":focus-visible");
    expect(css).toContain("prefers-reduced-motion");
    expect(css).toContain("@media (max-width: 760px)");
    expect(css).toContain("@media (max-width: 1150px)");
  });
  it("persists user typography preference without changing runtime policy",()=>{
    expect(app).toContain('localStorage.setItem("cairn.v2.fontSize"');
    expect(app).toContain('style.setProperty("--v2-font-size-base"');
    expect(css).toContain("font: var(--v2-font-size-base)/1.55");
  });
});
