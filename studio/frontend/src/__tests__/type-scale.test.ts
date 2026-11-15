/**
 * Studio GUI design contracts: the Wails entry loads only the workbench theme.
 * Legacy typography guards were coupled to a stylesheet no longer referenced by the app.
 */
import { describe, expect, it } from "vitest";
const fs = (globalThis as unknown as {process?: {getBuiltinModule?: (s:string)=>unknown}}).process?.getBuiltinModule?.("node:fs") as
  {readFileSync:(p:string,enc:"utf8")=>string}|undefined;
const cwd=(globalThis as unknown as {process?: {cwd?:()=>string}}).process?.cwd?.();
if(!fs||!cwd)throw new Error("需要 Node 内建 fs 与工作目录");
const read=(path:string)=>fs.readFileSync(cwd+"/"+path,"utf8");
const css=read("src/tokens.css");
const entry=read("src/main.tsx");
const app=read("src/App.tsx");
describe("Studio GUI workbench theme",()=>{
  it("only loads the workbench application and its own theme",()=>{
    expect(entry).toContain('from "./App"');
    expect(entry).toContain('import "./tokens.css"');
    expect(entry).not.toContain('import "./styles.css"');
    expect(entry).not.toContain('./v2/');
  });
  it("defines the frozen Cairn brand colors",()=>{
    for(const token of ["--wb-shell","--wb-side","--wb-paper","--wb-ink","--wb-action","--wb-font-size-base"])expect(css).toContain(token);
    expect(css).toContain("#b55c35");
  });
  it("supports visible focus, reduced motion and narrow windows",()=>{
    expect(css).toContain(":focus-visible");
    expect(css).toContain("prefers-reduced-motion");
    expect(css).toContain("@media (max-width: 760px)");
    expect(css).toContain("@media (max-width: 1150px)");
  });
  it("persists user typography preference without changing runtime policy",()=>{
    expect(app).toContain('localStorage.setItem("cairn.gui.fontSize"');
    expect(app).toContain('style.setProperty("--wb-font-size-base"');
    expect(css).toContain("font: var(--wb-font-size-base)/1.55");
  });
});
