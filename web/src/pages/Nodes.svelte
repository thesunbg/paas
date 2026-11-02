<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ClusterNode, type JoinToken, type NodesResponse, type StuckItem } from '../lib/api';
  import { href } from '../lib/router.svelte';
  import { session } from '../lib/session.svelte';
  import { mb, cpu, gb, relTime, absTime, pct, bytes, rate } from '../lib/format';
  import { poll } from '../lib/poll';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import Status from '../components/Status.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Modal from '../components/Modal.svelte';
  import Sparkline from '../components/Sparkline.svelte';
  import type { NodeMetricPoint } from '../lib/api';

  let data = $state<NodesResponse | null>(null);
  let error = $state('');
  let busy = $state('');
  let join = $state<JoinToken | null>(null);
  let joinOpen = $state(false);
  let joinError = $state('');

  let editing = $state<ClusterNode | null>(null);
  let editOpen = $state(false);
  let ocMem = $state(1.5);
  let ocCpu = $state(4);
  let reserved = $state(1024);
  let editError = $state('');

  let metrics = $state<Record<string, NodeMetricPoint[]>>({});
  let lastMetrics = 0;
  async function loadMetrics(ids: string[]) {
    lastMetrics = Date.now();
    const res = await Promise.all(ids.map((id) => api.nodeMetrics(id).then((m) => [id, m ?? []] as const).catch(() => [id, null] as const)));
    for (const [id, m] of res) if (m) metrics[id] = m;
  }

  async function load() {
    try {
      data = await api.nodes();
      error = '';
      if (Date.now() - lastMetrics > 60000) loadMetrics(data.nodes.map((n) => n.id));
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    return poll(load, 5000);
  });

  async function openJoin() {
    joinOpen = !joinOpen;
    if (joinOpen) {
      join = null;
      joinError = '';
      try {
        join = await api.createJoinToken(['worker']);
      } catch (e) {
        joinError = errorMessage(e);
      }
    }
  }

  async function setStatus(n: ClusterNode, status: 'online' | 'cordoned') {
    if (status === 'cordoned') {
      const ok = await confirmDialog({
        title: 'Cordon node',
        message: `No new instances will be scheduled on ${n.name}. Running instances keep running.`,
        confirmLabel: 'Cordon',
      });
      if (!ok) return;
    }
    busy = n.id;
    try {
      await api.updateNode(n.id, { status });
      toast.success(`${n.name} ${status === 'cordoned' ? 'cordoned' : 'is schedulable again'}`);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  let stuckNode = $state<ClusterNode | null>(null);
  let stuck = $state<StuckItem[]>([]);
  let stuckOpen = $state(false);

  async function remove(n: ClusterNode) {
    let items: StuckItem[] = [];
    try {
      items = (await api.nodeStuck(n.id)) ?? [];
    } catch (e) {
      return void toast.error(errorMessage(e));
    }
    if (items.length > 0) {
      stuckNode = n;
      stuck = items;
      stuckOpen = true;
      return;
    }
    const ok = await confirmDialog({ title: 'Remove node', message: `Remove ${n.name} from the cluster? Its agent must already be stopped.`, confirmLabel: 'Remove', danger: true, requireText: n.name });
    if (!ok) return;
    await doRemove(n, false);
  }

  async function forceRemove() {
    const n = stuckNode;
    if (!n) return;
    stuckOpen = false;
    const ok = await confirmDialog({
      title: 'Remove node anyway',
      message: `The ${stuck.length} item(s) listed keep their data on ${n.name}. Removing it starts them elsewhere with EMPTY data. Recover them first unless that data is truly gone.`,
      confirmLabel: 'Remove anyway',
      danger: true,
      requireText: n.name,
    });
    if (ok) await doRemove(n, true);
  }

  async function doRemove(n: ClusterNode, force: boolean) {
    try {
      await api.deleteNode(n.id, force);
      toast.success(`${n.name} removed`);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  // Stateful services of a node: drain with their volumes (node online), or restore them on other
  // nodes from their latest backups (node lost).
  let sfNode = $state<ClusterNode | null>(null);
  let sfItems = $state<StuckItem[]>([]);
  let sfOpen = $state(false);
  let sfBusy = $state(false);

  async function openStateful(n: ClusterNode) {
    try {
      sfItems = (await api.nodeStuck(n.id)) ?? [];
    } catch (e) {
      return void toast.error(errorMessage(e));
    }
    sfNode = n;
    sfOpen = true;
  }

  async function drain(n: ClusterNode, moveVolumes: boolean) {
    const ok = await confirmDialog({
      title: moveVolumes ? `Drain ${n.name} and move its volumes` : `Drain ${n.name}`,
      message: moveVolumes
        ? `${n.name} is cordoned, its stateless instances move without downtime, and each service with volumes moves to another node one at a time: it stops while its volumes are copied and checked, then starts on the new node. A failed move leaves it running on ${n.name}. Database servers stay.`
        : `${n.name} is cordoned and its stateless instances move without downtime. Services with volumes stay on it.`,
      confirmLabel: 'Drain',
      danger: moveVolumes,
    });
    if (!ok) return;
    sfBusy = true;
    try {
      const r = await api.drainNode(n.id, moveVolumes);
      const parts = [`${r.moving} instance(s) moving`];
      if (r.moving_stateful.length) parts.push(`volumes moving: ${r.moving_stateful.join(', ')}`);
      if (r.pinned_stateful.length) parts.push(`staying: ${r.pinned_stateful.join(', ')}`);
      toast.success(`Draining ${n.name}: ${parts.join('; ')}`);
      sfOpen = false;
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      sfBusy = false;
    }
  }

  async function recoverAll(n: ClusterNode) {
    const services = sfItems.filter((it) => it.kind === 'service');
    const ok = await confirmDialog({
      title: `Restore the services of ${n.name}`,
      message:
        `Each of the ${services.length} service(s) with volumes is restored on another online node from its newest off-site backup, one at a time. ` +
        `What it wrote after that backup is lost (the times are listed). Services without a backup are skipped. ` +
        `Only do this if ${n.name} is not coming back; its old volumes are kept and removed 7 days after it is back.`,
      confirmLabel: 'Restore',
      danger: true,
      requireText: n.name,
    });
    if (!ok) return;
    sfBusy = true;
    try {
      const r = await api.recoverNode(n.id, false);
      const msg = [`${r.started.length} restore(s) started`];
      if (r.skipped.length) msg.push(`skipped: ${r.skipped.map((x) => `${x.name} (${x.reason})`).join(', ')}`);
      if (r.skipped.length) toast.error(msg.join('; '));
      else toast.success(msg.join('; '));
      sfOpen = false;
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      sfBusy = false;
    }
  }

  const stuckLabel: Record<StuckItem['kind'], string> = { service: 'Service volumes', database: 'Database', 'shared-database': 'Database on a shared server' };

  function edit(n: ClusterNode) {
    editing = n;
    ocMem = n.overcommit_memory;
    ocCpu = n.overcommit_cpu;
    reserved = n.reserved_mb;
    editError = '';
    editOpen = true;
  }

  async function saveEdit(e: SubmitEvent) {
    e.preventDefault();
    if (!editing) return;
    if (ocMem < 1 || ocMem > 4) return void (editError = 'Memory overcommit must be between 1 and 4');
    if (ocCpu < 1 || ocCpu > 16) return void (editError = 'CPU overcommit must be between 1 and 16');
    try {
      await api.updateNode(editing.id, { overcommit_memory: Number(ocMem), overcommit_cpu: Number(ocCpu), reserved_mb: Number(reserved) });
      toast.success(`${editing.name} updated`);
      editOpen = false;
      await load();
    } catch (err) {
      editError = errorMessage(err);
    }
  }

  const ratio = (a: number, b: number) => (b > 0 ? a / b : 0);
  const level = (r: number) => (r > 0.9 ? 'crit' : r > 0.75 ? 'warn' : '');
  const stale = (n: ClusterNode) => Date.now() - Date.parse(n.last_seen_at) > 60_000;
  // Its agent still reports in (online or cordoned): its data can be moved, not restored from a backup.
  const alive = (n: ClusterNode) => n.status !== 'offline' && !stale(n);

  const totals = $derived.by(() => {
    const ns = data?.nodes ?? [];
    return {
      mem: ns.reduce((a, n) => a + n.memory_mb, 0),
      committed: ns.reduce((a, n) => a + n.committed_memory_mb, 0),
      capacity: ns.reduce((a, n) => a + n.capacity_memory_mb, 0),
      used: ns.reduce((a, n) => a + n.used_by_apps_mb, 0),
      instances: ns.reduce((a, n) => a + n.instances, 0),
      cores: ns.reduce((a, n) => a + n.cpu_cores, 0),
    };
  });
</script>

<div class="page wide">
  <div class="page-head">
    <div>
      <h1>Nodes</h1>
      <p class="sub">Machines in the cluster and how densely they are packed.</p>
    </div>
    <button class="primary" onclick={openJoin} aria-expanded={joinOpen}><Icon name="plus" />Add node</button>
  </div>

  {#if !session.me?.user.is_admin}
    <div class="alert error">Administrator only.</div>
  {:else}
    {#if joinOpen}
      <div class="card join">
        <div class="card-head">
          <h3>Add a node</h3>
          <button class="ghost icon sm" aria-label="Close" onclick={() => (joinOpen = false)}><Icon name="x" /></button>
        </div>
        <div class="card-body stack">
          <ErrorMsg error={joinError} />
          {#if join}
            <p class="small muted">Run this on a fresh Linux machine (Debian/Ubuntu, systemd, cgroup v2 recommended). It installs the agent and joins the cluster as a worker. Change <code>--roles</code> to <code>worker,build</code> or <code>edge</code> as needed.</p>
            <div class="cmd">
              <pre class="log">{join.command}</pre>
              <div class="copy"><CopyButton text={join.command} label="Copy command" small /></div>
            </div>
            <p class="hint">The token is single-use and expires at {new Date(join.expires_at).toLocaleString()}. The node appears below within a few seconds of starting.</p>
          {:else if !joinError}
            <Loading />
          {/if}
        </div>
      </div>
    {/if}

    <ErrorMsg {error} onretry={load} />

    {#if !data && !error}
      <Loading rows={3} />
    {:else if data}
      <div class="stats">
        <div class="stat card"><span class="lbl">Nodes</span><span class="val">{data.nodes.length}</span></div>
        <div class="stat card"><span class="lbl">CPU cores</span><span class="val">{totals.cores}</span></div>
        <div class="stat card"><span class="lbl">Memory</span><span class="val">{mb(totals.mem)}</span></div>
        <div class="stat card">
          <span class="lbl">Committed / capacity</span><span class="val">{pct(ratio(totals.committed, totals.capacity))}</span>
          <span class="bar {level(ratio(totals.committed, totals.capacity))}"><span style="width:{Math.min(100, ratio(totals.committed, totals.capacity) * 100)}%"></span></span>
        </div>
        <div class="stat card"><span class="lbl">Instances</span><span class="val">{totals.instances}</span></div>
        <div class="stat card"><span class="lbl">Sleeping services</span><span class="val purple">{data.sleeping_services}</span><span class="faint small">0 MB while asleep</span></div>
      </div>

      {#if data.nodes.length === 0}
        <div class="card empty">
          <Icon name="server" size={28} />
          <p>No nodes have joined yet.</p>
          <button class="primary" onclick={openJoin}>Show join command</button>
        </div>
      {:else}
        <div class="card table-wrap">
          <table>
            <thead>
              <tr>
                <th>Node</th>
                <th>Status</th>
                <th>CPU <span class="faint">· 24h</span></th>
                <th>Memory</th>
                <th style="min-width:180px">Committed / capacity <span class="faint">· 24h</span></th>
                <th class="right">Used by apps</th>
                <th class="right">Overcommit</th>
                <th class="right">Instances</th>
                <th>Network <span class="faint">· 24h</span></th>
                <th>Disk</th>
                <th class="right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {#each data.nodes as n (n.id)}
                {@const memR = ratio(n.committed_memory_mb, n.capacity_memory_mb)}
                {@const cpuR = ratio(n.committed_cpu_millis, n.capacity_cpu_millis)}
                {@const diskR = ratio(n.usage.disk_used_gb, n.usage.disk_total_gb)}
                <tr>
                  <td>
                    <div class="name">{n.name}</div>
                    <div class="mono small faint">{n.address}</div>
                    <div class="row nowrap" style="gap:4px;margin-top:4px">
                      {#each n.roles as r (r)}<span class="badge outline">{r}</span>{/each}
                    </div>
                  </td>
                  <td>
                    <Status status={stale(n) && n.status === 'online' ? 'offline' : n.status} label={stale(n) && n.status === 'online' ? 'unreachable' : n.status} />
                    <div class="faint small nowrap" title={Object.entries(n.capabilities ?? {}).map(([k, v]) => `${k}: ${v}`).join(', ')}>v{n.version}{n.capabilities?.oci_runtime ? ` · ${n.capabilities.oci_runtime}` : ''}</div>
                    <div class="faint small nowrap" title={absTime(n.last_seen_at)}>seen {relTime(n.last_seen_at)}</div>
                  </td>
                  <td class="nowrap">
                    <div>{n.cpu_cores} cores · {n.usage.cpu_percent.toFixed(1)}%</div>
                    <div class="faint small">load {n.usage.load1.toFixed(2)}</div>
                    {#if metrics[n.id]}
                      {@const m = metrics[n.id]}
                      <div class="trend" title="CPU %, last 24 h">
                        <Sparkline values={m.map((p) => p.cpu_percent)} times={m.map((p) => Date.parse(p.ts))} label="{n.name} CPU, 24 h" max={100} width={96} format={(v) => `${v.toFixed(0)}%`} />
                      </div>
                    {/if}
                  </td>
                  <td class="nowrap">
                    <div>{mb(n.memory_mb)}</div>
                    <div class="faint small">{mb(n.usage.memory_used_mb)} used</div>
                    <div class="faint small">{mb(n.reserved_mb)} reserved</div>
                  </td>
                  <td>
                    <div class="dens">
                      <div class="between small nowrap"><span>RAM {mb(n.committed_memory_mb)} / {mb(n.capacity_memory_mb)}</span><span class="faint">{pct(memR)}</span></div>
                      <span class="bar {level(memR)}" title="Committed memory requests vs schedulable capacity">
                        <span style="width:{Math.min(100, memR * 100)}%"></span>
                      </span>
                      <div class="between small nowrap"><span>CPU {cpu(n.committed_cpu_millis)} / {cpu(n.capacity_cpu_millis)}</span><span class="faint">{pct(cpuR)}</span></div>
                      <span class="bar {level(cpuR)}"><span style="width:{Math.min(100, cpuR * 100)}%"></span></span>
                      {#if metrics[n.id]}
                        {@const m = metrics[n.id]}
                        <div class="trend" title="Committed memory, last 24 h">
                          <Sparkline values={m.map((p) => p.memory_committed_mb)} times={m.map((p) => Date.parse(p.ts))} label="{n.name} committed memory, 24 h" width={110} format={(v) => mb(v)} />
                        </div>
                      {/if}
                    </div>
                  </td>
                  <td class="right nowrap">
                    {mb(n.used_by_apps_mb)}
                    <div class="faint small">{n.committed_memory_mb ? pct(ratio(n.used_by_apps_mb, n.committed_memory_mb)) + ' of committed' : ''}</div>
                  </td>
                  <td class="right nowrap">
                    <div title="Committed memory / (physical − reserved)">{n.overcommit_ratio.toFixed(2)}×</div>
                    <div class="faint small">max {n.overcommit_memory}× mem</div>
                    <div class="faint small">max {n.overcommit_cpu}× cpu</div>
                  </td>
                  <td class="right">{n.instances}</td>
                  <td class="nowrap">
                    <div class="small" title="Physical interfaces, now">↓ {rate(n.usage.net_rx_bps ?? 0)} · ↑ {rate(n.usage.net_tx_bps ?? 0)}</div>
                    {#if n.traffic}
                      <div class="faint small" title="Received / sent{n.traffic.since ? ` since ${n.traffic.since}` : ' in the last 30 days'} (UTC days)">30 d: ↓ {bytes(n.traffic.rx_30d)} ↑ {bytes(n.traffic.tx_30d)}</div>
                    {/if}
                    {#if metrics[n.id]}
                      {@const m = metrics[n.id]}
                      <div class="trend" title="Traffic (in + out) per minute, last 24 h">
                        <Sparkline values={m.map((p) => ((p.net_rx_bytes ?? 0) + (p.net_tx_bytes ?? 0)) / 60)} times={m.map((p) => Date.parse(p.ts))} label="{n.name} network, 24 h" width={96} format={(v) => rate(v)} />
                      </div>
                    {/if}
                  </td>
                  <td class="nowrap">
                    <div class="small">{gb(n.usage.disk_used_gb)} / {gb(n.usage.disk_total_gb)}</div>
                    <span class="bar {level(diskR)}" style="width:80px;min-width:80px"><span style="width:{Math.min(100, diskR * 100)}%"></span></span>
                  </td>
                  <td class="right nowrap">
                    <div class="acts">
                    {#if n.status === 'online'}
                      <button class="sm" onclick={() => setStatus(n, 'cordoned')} disabled={busy === n.id}>Cordon</button>
                    {:else}
                      <button class="sm" onclick={() => setStatus(n, 'online')} disabled={busy === n.id}>Uncordon</button>
                    {/if}
                    <button class="sm" onclick={() => openStateful(n)} title="Services with volumes on this node: drain them, or restore them elsewhere if the node is lost">{alive(n) ? 'Drain…' : 'Recover…'}</button>
                    <button class="sm" onclick={() => edit(n)}>Edit</button>
                    {#if n.status !== 'online'}
                      <button class="sm danger" aria-label="Remove {n.name}" onclick={() => remove(n)}><Icon name="trash" size={12} />Remove</button>
                    {/if}
                    </div>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        <p class="hint" style="margin-top:8px">
          Capacity = (memory − reserved) × memory overcommit for burstable workloads. Committed is the sum of instance memory requests; “used by apps” is actual usage.
        </p>
      {/if}
    {/if}
  {/if}
</div>

<Modal bind:open={stuckOpen} title="Data still on {stuckNode?.name ?? 'this node'}" width={560}>
  <div class="stack small">
    <p>These keep their data on this node. Recover each one on another node first (from its newest off-site backup), then remove the node.</p>
    <table>
      <thead><tr><th>What</th><th>Project</th><th>Newest off-site backup</th></tr></thead>
      <tbody>
        {#each stuck as it (it.kind + it.id)}
          <tr>
            <td>
              <a href={href(it.kind === 'service' ? `/services/${it.id}/settings` : `/databases/${it.id}`)} onclick={() => (stuckOpen = false)}>{it.name}</a>
              <div class="faint">{stuckLabel[it.kind]}</div>
            </td>
            <td>{it.project ?? ''}</td>
            <td>{#if it.backup}<span title={absTime(it.backup)}>{relTime(it.backup)}</span>{:else}<span class="badge red">none</span>{/if}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  {#snippet footer()}
    <button type="button" onclick={() => (stuckOpen = false)}>Cancel</button>
    <button type="button" class="danger" onclick={forceRemove}>Remove anyway…</button>
  {/snippet}
</Modal>

<Modal bind:open={sfOpen} title="Stateful services on {sfNode?.name ?? 'this node'}" width={600}>
  <div class="stack small">
    {#if sfNode && alive(sfNode)}
      <p>These keep their data on this node. A drain moves stateless instances only, unless you also move the volumes (each service is down while its volumes are copied).</p>
    {:else}
      <p><span class="badge red">unreachable</span> If this node is not coming back, restore its services on other nodes from their newest off-site backups. Databases have their own recovery (open each one).</p>
    {/if}
    {#if sfItems.length === 0}
      <p class="muted">Nothing keeps its data only on this node.</p>
    {:else}
      <table>
        <thead><tr><th>What</th><th>Project</th><th>Newest off-site backup</th></tr></thead>
        <tbody>
          {#each sfItems as it (it.kind + it.id)}
            <tr>
              <td>
                <a href={href(it.kind === 'service' ? `/services/${it.id}/settings` : `/databases/${it.id}`)} onclick={() => (sfOpen = false)}>{it.name}</a>
                <div class="faint">{stuckLabel[it.kind]}</div>
              </td>
              <td>{it.project ?? ''}</td>
              <td>{#if it.backup}<span title={absTime(it.backup)}>{relTime(it.backup)}</span>{:else}<span class="badge red">none</span>{/if}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
  {#snippet footer()}
    <button type="button" onclick={() => (sfOpen = false)}>Cancel</button>
    {#if sfNode && alive(sfNode)}
      <button type="button" onclick={() => sfNode && drain(sfNode, false)} disabled={sfBusy}>Drain</button>
      <button type="button" class="danger" onclick={() => sfNode && drain(sfNode, true)} disabled={sfBusy || !sfItems.some((it) => it.kind === 'service')}>Drain and move volumes</button>
    {:else}
      <button type="button" class="danger" onclick={() => sfNode && recoverAll(sfNode)} disabled={sfBusy || !sfItems.some((it) => it.kind === 'service')}>Restore on other nodes…</button>
    {/if}
  {/snippet}
</Modal>

<Modal bind:open={editOpen} title="Edit {editing?.name ?? 'node'}" width={460}>
  <form id="node-edit" class="stack" onsubmit={saveEdit}>
    <div class="grid-2">
      <label class="field">
        <span>Memory overcommit</span>
        <input type="number" min="1" max="4" step="0.1" bind:value={ocMem} />
        <span class="hint">1 – 4×</span>
      </label>
      <label class="field">
        <span>CPU overcommit</span>
        <input type="number" min="1" max="16" step="0.5" bind:value={ocCpu} />
        <span class="hint">1 – 16×</span>
      </label>
    </div>
    <label class="field">
      <span>Reserved memory (MB)</span>
      <input type="number" min="0" step="64" bind:value={reserved} />
      <span class="hint">Kept free for the OS and agent; never scheduled.</span>
    </label>
    {#if editing}
      {@const cap = Math.round((editing.memory_mb - reserved) * ocMem)}
      <p class="small muted">Resulting memory capacity: <b>{mb(cap)}</b> (currently {mb(editing.capacity_memory_mb)}, committed {mb(editing.committed_memory_mb)}).</p>
    {/if}
    <ErrorMsg error={editError} />
  </form>
  {#snippet footer()}
    <button type="button" onclick={() => (editOpen = false)}>Cancel</button>
    <button type="submit" form="node-edit" class="primary">Save</button>
  {/snippet}
</Modal>

<style>
  .wide {
    max-width: 1600px;
  }
  .join {
    margin-bottom: 20px;
  }
  .cmd {
    position: relative;
  }
  .cmd .log {
    padding-right: 140px;
  }
  .copy {
    position: absolute;
    top: 8px;
    right: 8px;
  }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 10px;
    margin-bottom: 16px;
  }
  .stat {
    padding: 12px 14px;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .lbl {
    color: var(--text-faint);
    font-size: 12px;
  }
  .val {
    font-size: 20px;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
  .val.purple {
    color: var(--purple);
  }
  .name {
    font-weight: 600;
  }
  .trend {
    margin-top: 6px;
  }
  .dens {
    display: flex;
    flex-direction: column;
    gap: 4px;
    min-width: 170px;
  }
  .acts {
    display: flex;
    flex-direction: column;
    gap: 4px;
    align-items: stretch;
  }
  td {
    vertical-align: top;
  }
  @media (max-width: 600px) {
    .cmd .log {
      padding-right: 14px;
      padding-top: 44px;
    }
  }
</style>
