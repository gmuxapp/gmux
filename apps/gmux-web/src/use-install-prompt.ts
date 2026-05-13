import { useState, useEffect } from 'preact/hooks'

export function useInstallPrompt() {
  const [prompt, setPrompt] = useState<BeforeInstallPromptEvent | null>(null)

  useEffect(() => {
    const handler = (e: Event) => {
      e.preventDefault()
      setPrompt(e as BeforeInstallPromptEvent)
    }
    window.addEventListener('beforeinstallprompt', handler)
    return () => window.removeEventListener('beforeinstallprompt', handler)
  }, [])

  const trigger = () => {
    if (!prompt) {
      alert(
        'Install prompt not available.\n\n' +
        'This can happen if:\n' +
        '• The app is already installed\n' +
        '• The browser has dismissed the prompt (try again later)\n' +
        '• You\'re not on HTTPS or localhost'
      )
      return
    }
    prompt.prompt()
    prompt.userChoice.then(() => setPrompt(null))
  }

  return { trigger }
}
