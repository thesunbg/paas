import { api, type Me, type Team } from './api';

const TEAM_KEY = 'paas.team';

function readTeam(): string {
  try {
    return localStorage.getItem(TEAM_KEY) ?? '';
  } catch {
    return '';
  }
}

export const session = $state<{ me: Me | null; teamId: string; loaded: boolean }>({ me: null, teamId: readTeam(), loaded: false });

export function currentTeam(): Team | undefined {
  const teams = session.me?.teams ?? [];
  return teams.find((t) => t.id === session.teamId) ?? teams[0];
}

export function setTeam(id: string) {
  session.teamId = id;
  try {
    localStorage.setItem(TEAM_KEY, id);
  } catch {
    /* storage unavailable */
  }
}

export async function loadMe() {
  const me = await api.me();
  session.me = me;
  if (!me.teams.some((t) => t.id === session.teamId) && me.teams.length) setTeam(me.teams[0].id);
  session.loaded = true;
  return me;
}

const rank: Record<string, number> = { viewer: 0, developer: 1, admin: 2, owner: 3 };

/** Whether the user has at least `min` in the current team (system admins are owners everywhere). */
export function can(min: 'viewer' | 'developer' | 'admin' | 'owner', teamId?: string): boolean {
  if (session.me?.user.is_admin) return true;
  const t = teamId ? session.me?.teams.find((x) => x.id === teamId) : currentTeam();
  return rank[t?.role ?? 'viewer'] >= rank[min];
}
