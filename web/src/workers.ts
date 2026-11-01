// 为 monaco-vscode-api 的主线程编辑器与 TextMate 分词器提供 web worker。
// Vite 的 ?worker 会把入口单独打包，这里按 label 返回对应的 worker 实例。
import EditorWorker from '@codingame/monaco-vscode-api/workers/editor.worker?worker'
import TextMateWorker from '@codingame/monaco-vscode-textmate-service-override/worker?worker'

declare global {
  interface Window {
    MonacoEnvironment?: {
      getWorker?: (moduleId: string, label: string) => Worker | undefined
    }
  }
}

window.MonacoEnvironment = {
  getWorker(_moduleId: string, label: string): Worker | undefined {
    if (label === 'TextMateWorker') return new TextMateWorker()
    return new EditorWorker()
  }
}
