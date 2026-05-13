/** Build-time version injected by Vite define. Matches the VERSION env var used by Go ldflags. */
declare const __GMUX_VERSION__: string

/** PWA install prompt — not yet in lib.dom.d.ts */
interface BeforeInstallPromptEvent extends Event {
  prompt(): Promise<void>
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>
}
