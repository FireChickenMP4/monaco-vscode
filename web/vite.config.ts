import { defineConfig } from 'vite'
import { fileURLToPath } from 'node:url'

// monaco-vscode-api 会把 VS Code 的 css 以字符串方式注入 workbench，
// 这里把相关包里的 css 全部按 ?inline 处理（官方 demo 的做法）。
function inlineVscodeCss() {
  return {
    name: 'inline-vscode-css',
    enforce: 'pre' as const,
    async resolveId(this: any, source: string, importer: string | undefined, options: any): Promise<any> {
      const resolved: any = await this.resolve(source, importer, options)
      if (!resolved) return undefined
      if (/node_modules[\\/](@codingame[\\/]monaco-vscode[^\\/]*|vscode|monaco-editor)[\\/].*\.css$/.test(resolved.id)) {
        return { ...resolved, id: resolved.id + '?inline' }
      }
      return undefined
    }
  }
}

export default defineConfig({
  plugins: [inlineVscodeCss()],
  build: {
    target: 'esnext',
    outDir: fileURLToPath(new URL('../public', import.meta.url)),
    emptyOutDir: false,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 8192,
    sourcemap: false
  },
  worker: {
    format: 'es'
  },
  resolve: {
    dedupe: ['vscode', 'monaco-editor', '@codingame/monaco-vscode-api']
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:8080'
    }
  }
})
