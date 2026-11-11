/** Vite 配置：React + Wails bindings 插件；测试模式跳过 Wails 插件。 */
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import wails from "@wailsio/runtime/plugins/vite";

export default defineConfig(({ mode }) => ({
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  plugins: mode === "test" ? [react()] : [react(), wails("./bindings")],
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    globals: true,
  },
}));
