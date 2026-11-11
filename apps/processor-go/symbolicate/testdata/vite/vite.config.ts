import { defineConfig } from "vite";
export default defineConfig({ build: { sourcemap: true, minify: "esbuild", target: "es2020", modulePreload: false } });
