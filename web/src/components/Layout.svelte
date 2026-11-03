<script lang="ts">
  import type { Snippet } from 'svelte';
  import { api } from '../lib/api';
  import { session, currentTeam, setTeam } from '../lib/session.svelte';
  import { href, navigate } from '../lib/router.svelte';
  import { toast } from '../lib/toast.svelte';
  import Icon from './Icon.svelte';
  import LangSwitch from './LangSwitch.svelte';
  import { t } from '../lib/i18n.svelte';

  let { section, onlogout, children }: { section: string; onlogout: () => void; children: Snippet } = $props();

  let navOpen = $state(false);
  let userOpen = $state(false);
  let teamOpen = $state(false);

  const me = $derived(session.me!);
  const team = $derived(currentTeam());
  const isAdmin = $derived(me.user.is_admin);
  const active = $derived(section === 'services' || section === 'databases' || section === 'deployments' ? 'projects' : section);

  const nav = $derived(
    [
      { id: 'projects', label: t('Projects'), icon: 'folder', path: '/' },
      ...(isAdmin
        ? [
            { id: 'nodes', label: t('Nodes'), icon: 'server', path: '/nodes' },
            { id: 'batches', label: t('Batches'), icon: 'layers', path: '/batches' },
          ]
        : []),
      { id: 'settings', label: t('Settings'), icon: 'settings', path: '/settings' },
    ],
  );

  async function logout() {
    userOpen = false;
    try {
      await api.logout();
    } catch {
      /* ignore */
    }
    session.me = null;
    toast.info(t('Signed out'));
    location.hash = '#/';
    onlogout();
  }

  function pickTeam(id: string) {
    setTeam(id);
    teamOpen = false;
    navigate('/');
  }

  function closeMenus(e: MouseEvent) {
    const el = e.target as HTMLElement;
    if (!el.closest('.menu-root')) {
      userOpen = false;
      teamOpen = false;
    }
  }
  function onkey(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      userOpen = false;
      teamOpen = false;
      navOpen = false;
    }
  }

  // Close the mobile nav whenever the route changes.
  $effect(() => {
    void section;
    navOpen = false;
  });

  const initials = $derived((me.user.name || me.user.email).slice(0, 2).toUpperCase());
</script>

<svelte:window onclick={closeMenus} onkeydown={onkey} onhashchange={() => (navOpen = false)} />

<div class="shell">
  <aside class="sidebar" class:open={navOpen}>
    <a class="brand" href={href('/')}>
      <span class="logo" aria-hidden="true"><svg viewBox="0 0 16 16" width="14" height="14"><path d="M2 13 8 2.5 14 13z" fill="currentColor" /></svg></span>
      <span>PaaS</span>
    </a>
    <nav aria-label={t('Main')}>
      {#each nav as n (n.id)}
        <a class="nav" class:active={active === n.id} href={href(n.path)} aria-current={active === n.id ? 'page' : undefined}>
          <Icon name={n.icon} />{n.label}
        </a>
      {/each}
    </nav>
    <div class="side-foot faint small">
      <a href="/api/v1/setup" class="faint" target="_blank" rel="noreferrer">API</a> · v0.1
    </div>
  </aside>
  {#if navOpen}
    <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
    <div class="scrim" onclick={() => (navOpen = false)}></div>
  {/if}

  <div class="main">
    <header class="topbar">
      <button class="ghost icon burger" aria-label={t('Open navigation')} aria-expanded={navOpen} onclick={() => (navOpen = !navOpen)}><Icon name="menu" /></button>

      <div class="menu-root">
        <button class="ghost team-btn" aria-haspopup="menu" aria-expanded={teamOpen} onclick={() => ((teamOpen = !teamOpen), (userOpen = false))}>
          <span class="team-av" aria-hidden="true">{(team?.name ?? '?').slice(0, 1).toUpperCase()}</span>
          <span class="truncate team-name">{team?.name ?? t('No team')}</span>
          {#if team?.role}<span class="badge outline hide-sm">{t(team.role)}</span>{/if}
          <Icon name="chevdown" size={14} />
        </button>
        {#if teamOpen}
          <div class="menu" role="menu" aria-label={t('Switch team')}>
            <div class="menu-label">{t('Teams')}</div>
            {#each me.teams as tm (tm.id)}
              <button role="menuitemradio" aria-checked={tm.id === team?.id} class="item" onclick={() => pickTeam(tm.id)}>
                <span class="team-av sm" aria-hidden="true">{tm.name.slice(0, 1).toUpperCase()}</span>
                <span class="truncate" style="flex:1">{tm.name}</span>
                {#if tm.id === team?.id}<Icon name="check" size={14} />{/if}
              </button>
            {/each}
            <div class="sep"></div>
            <a class="item" role="menuitem" href={href('/settings/members')} onclick={() => (teamOpen = false)}><Icon name="user" size={14} />{t('Manage members')}</a>
          </div>
        {/if}
      </div>

      <div class="spacer"></div>

      <LangSwitch />

      <div class="menu-root">
        <button class="ghost user-btn" aria-haspopup="menu" aria-expanded={userOpen} aria-label={t('User menu')} onclick={() => ((userOpen = !userOpen), (teamOpen = false))}>
          <span class="avatar" aria-hidden="true">{initials}</span>
          <span class="hide-sm truncate" style="max-width:180px">{me.user.email}</span>
          <Icon name="chevdown" size={14} />
        </button>
        {#if userOpen}
          <div class="menu right" role="menu">
            <div class="menu-head">
              <div style="font-weight:500">{me.user.name}</div>
              <div class="faint small truncate">{me.user.email}</div>
              {#if me.user.is_admin}<span class="badge blue" style="margin-top:6px">{t('administrator')}</span>{/if}
            </div>
            <div class="sep"></div>
            <a class="item" role="menuitem" href={href('/settings/tokens')} onclick={() => (userOpen = false)}><Icon name="key" size={14} />{t('API tokens')}</a>
            <a class="item" role="menuitem" href={href('/settings/password')} onclick={() => (userOpen = false)}><Icon name="settings" size={14} />{t('Change password')}</a>
            <div class="sep"></div>
            <button class="item" role="menuitem" onclick={logout}><Icon name="logout" size={14} />{t('Log out')}</button>
          </div>
        {/if}
      </div>
    </header>
    <main>
      {@render children()}
    </main>
  </div>
</div>

<style>
  .shell {
    display: flex;
    min-height: 100vh;
  }
  .sidebar {
    width: var(--sidebar-w);
    flex: none;
    border-right: 1px solid var(--border);
    background: var(--bg);
    display: flex;
    flex-direction: column;
    position: sticky;
    top: 0;
    height: 100vh;
    padding: 12px 10px;
    z-index: 40;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 10px;
    font-weight: 700;
    font-size: 15px;
    color: var(--text);
    padding: 6px 8px 16px;
    letter-spacing: -0.02em;
  }
  .brand:hover {
    text-decoration: none;
  }
  .logo {
    width: 26px;
    height: 26px;
    border-radius: 7px;
    background: var(--text);
    color: var(--bg);
    display: inline-flex;
    align-items: center;
    justify-content: center;
  }
  nav {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .nav {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 7px 10px;
    border-radius: var(--radius-sm);
    color: var(--text-dim);
    font-weight: 500;
    font-size: 13.5px;
  }
  .nav:hover {
    background: var(--bg-hover);
    color: var(--text);
    text-decoration: none;
  }
  .nav.active {
    background: var(--bg-elev-2);
    color: var(--text);
    box-shadow: inset 0 0 0 1px var(--border);
  }
  .side-foot {
    margin-top: auto;
    padding: 8px 10px;
  }
  .main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .topbar {
    height: var(--topbar-h);
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 0 16px;
    border-bottom: 1px solid var(--border);
    background: color-mix(in srgb, var(--bg) 85%, transparent);
    backdrop-filter: blur(8px);
    position: sticky;
    top: 0;
    z-index: 30;
  }
  .burger {
    display: none;
  }
  .menu-root {
    position: relative;
    min-width: 0;
  }
  .team-btn,
  .user-btn {
    gap: 8px;
    padding: 0 8px;
    max-width: 100%;
  }
  .team-name {
    max-width: 220px;
  }
  .team-av {
    width: 22px;
    height: 22px;
    border-radius: 6px;
    background: linear-gradient(135deg, #6366f1, #3b82f6);
    color: #fff;
    font-size: 11px;
    font-weight: 700;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex: none;
  }
  .team-av.sm {
    width: 18px;
    height: 18px;
    font-size: 10px;
  }
  .avatar {
    width: 26px;
    height: 26px;
    border-radius: 50%;
    background: linear-gradient(135deg, #10b981, #3b82f6);
    color: #fff;
    font-size: 11px;
    font-weight: 700;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex: none;
  }
  .menu {
    position: absolute;
    top: calc(100% + 6px);
    left: 0;
    min-width: 240px;
    background: var(--bg-elev);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
    padding: 4px;
    z-index: 50;
  }
  .menu.right {
    left: auto;
    right: 0;
  }
  .menu-label {
    font-size: 11px;
    color: var(--text-faint);
    padding: 6px 8px 4px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .menu-head {
    padding: 8px;
  }
  .item {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    height: 32px;
    padding: 0 8px;
    border: 0;
    background: transparent;
    color: var(--text);
    border-radius: var(--radius-sm);
    font-size: 13px;
    font-weight: 400;
    justify-content: flex-start;
    text-align: left;
  }
  .item:hover {
    background: var(--bg-hover);
    text-decoration: none;
  }
  .sep {
    height: 1px;
    background: var(--border);
    margin: 4px 0;
  }
  main {
    flex: 1;
    min-width: 0;
  }
  .scrim {
    display: none;
  }

  @media (max-width: 860px) {
    .sidebar {
      position: fixed;
      left: 0;
      top: 0;
      bottom: 0;
      transform: translateX(-100%);
      transition: transform 0.18s ease;
      background: var(--bg-elev);
      box-shadow: var(--shadow);
    }
    .sidebar.open {
      transform: none;
    }
    .scrim {
      display: block;
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.5);
      z-index: 35;
    }
    .burger {
      display: inline-flex;
    }
    .topbar {
      padding: 0 8px;
    }
    .team-name {
      max-width: 140px;
    }
  }
</style>
