// Attention helpers for incoming driver ride offers: a short WebAudio chirp
// and a flashing document.title. No audio assets, no deps — pure Web APIs.
// Everything is wrapped so a browser that blocks audio before a user gesture
// (autoplay policy) fails silently rather than throwing.

// playChirp emits a ~0.4s two-tone attention chirp via a throwaway
// AudioContext. Safe to call repeatedly; a blocked context is swallowed.
export function playChirp(): void {
  try {
    const Ctx: typeof AudioContext | undefined =
      window.AudioContext || (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (!Ctx) return;
    const ctx = new Ctx();
    const now = ctx.currentTime;
    const gain = ctx.createGain();
    gain.connect(ctx.destination);
    gain.gain.setValueAtTime(0.0001, now);
    gain.gain.exponentialRampToValueAtTime(0.14, now + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, now + 0.42);

    const tone = (freq: number, at: number, dur: number) => {
      const osc = ctx.createOscillator();
      osc.type = "sine";
      osc.frequency.value = freq;
      osc.connect(gain);
      osc.start(now + at);
      osc.stop(now + at + dur);
    };
    tone(880, 0, 0.18); // first note
    tone(1175, 0.2, 0.2); // second, higher note
    window.setTimeout(() => {
      ctx.close().catch(() => {});
    }, 650);
  } catch {
    // Autoplay blocked / no audio device — ignore.
  }
}

// ---- title flashing ----

let flashTimer: number | null = null;
let savedTitle = "";
let focusHandler: (() => void) | null = null;

// startTitleFlash alternates document.title with `message` to draw attention
// when the tab is backgrounded. It self-restores as soon as the window regains
// focus (the driver is now looking), and is idempotent.
export function startTitleFlash(message: string): void {
  if (flashTimer != null) return;
  savedTitle = document.title;
  let showing = false;
  flashTimer = window.setInterval(() => {
    document.title = showing ? savedTitle : message;
    showing = !showing;
  }, 900);
  focusHandler = () => stopTitleFlash();
  window.addEventListener("focus", focusHandler);
}

// stopTitleFlash halts flashing and restores the original title.
export function stopTitleFlash(): void {
  if (flashTimer == null) return;
  clearInterval(flashTimer);
  flashTimer = null;
  document.title = savedTitle;
  if (focusHandler) {
    window.removeEventListener("focus", focusHandler);
    focusHandler = null;
  }
}
