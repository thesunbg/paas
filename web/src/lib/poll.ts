// Polls `fn` every `ms` while the tab is visible; returns a stop function.
export function poll(fn: () => unknown, ms: number): () => void {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let stopped = false;
  let running = false;
  const tick = async () => {
    timer = null;
    if (stopped) return;
    if (document.visibilityState === 'visible' && !running) {
      running = true;
      try {
        await fn();
      } catch {
        /* errors are surfaced by the caller */
      } finally {
        running = false;
      }
    }
    if (!stopped) timer = setTimeout(tick, ms);
  };
  const onVis = () => {
    if (document.visibilityState === 'visible' && !stopped) {
      if (timer) clearTimeout(timer);
      tick();
    }
  };
  document.addEventListener('visibilitychange', onVis);
  timer = setTimeout(tick, ms);
  return () => {
    stopped = true;
    if (timer) clearTimeout(timer);
    document.removeEventListener('visibilitychange', onVis);
  };
}
