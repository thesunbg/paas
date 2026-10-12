// Imperative confirmation dialog: `if (await confirmDialog({...})) ...`.
export interface ConfirmOptions {
  title: string;
  message: string;
  confirmLabel?: string;
  danger?: boolean;
  /** When set, the user must type this text to enable the confirm button. */
  requireText?: string;
}

interface Pending extends ConfirmOptions {
  resolve: (ok: boolean) => void;
}

export const confirmState = $state<{ current: Pending | null }>({ current: null });

export function confirmDialog(opts: ConfirmOptions): Promise<boolean> {
  confirmState.current?.resolve(false);
  return new Promise((resolve) => {
    confirmState.current = { ...opts, resolve };
  });
}

export function settleConfirm(ok: boolean) {
  const c = confirmState.current;
  confirmState.current = null;
  c?.resolve(ok);
}
