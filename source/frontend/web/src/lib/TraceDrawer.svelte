<script lang="ts">
  import { app } from './state.svelte';
  import Chip from './Chip.svelte';

  function tokens(t: string): string[] {
    return t ? t.split(',').map((s) => s.trim()).filter(Boolean) : [];
  }
  const latest = $derived(app.trace[0]);
</script>

<button class="fab" onclick={() => (app.drawer = !app.drawer)} aria-expanded={app.drawer} aria-label="Toggle request trace">
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 12h5l3-7 4 14 3-7h1" /></svg>
  trace{#if latest && latest.hops > 0}<b>{latest.hops}</b>{/if}
</button>

{#if app.drawer}
  <aside class="drawer fade-in" aria-label="Request trace">
    <div class="head">
      <h3>Request trace <span class="muted">(last {app.trace.length})</span></h3>
      <button class="btn sm ghost" onclick={() => (app.drawer = false)}>Close</button>
    </div>
    {#if app.trace.length === 0}
      <p class="muted">No requests yet.</p>
    {/if}
    <ul>
      {#each app.trace as e (e.id)}
        <li>
          <div class="line">
            <span class="kind k-{e.kind}">{e.kind === 'page' ? 'PAGE' : e.method}</span>
            <code class="path">{e.path}</code>
            <span class="status" class:bad={typeof e.status === 'string' || Number(e.status) >= 400 && Number(e.status) !== 101}>{e.status}</span>
          </div>
          <div class="chips2">
            {#each tokens(e.trace) as t}<Chip token={t} />{/each}
            {#if !e.trace}<span class="muted small">no trace header</span>{/if}
          </div>
          {#if e.trace}
            <div class="hops" class:ok={e.hops >= 2}>
              app hops: {e.hops} {#if e.hops >= 2}<span aria-label="at least two app containers">&#10003;</span>{/if}
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  </aside>
{/if}

<style>
  .fab {
    position: fixed; right: 12px; bottom: calc(12px + env(safe-area-inset-bottom)); z-index: 80;
    display: inline-flex; gap: 6px; align-items: center;
    background: var(--text); color: var(--bg); border: 0; border-radius: 999px;
    padding: 10px 14px; font: inherit; font-weight: 800; font-size: 0.85rem; cursor: pointer;
    box-shadow: var(--shadow); min-height: 40px;
  }
  .fab b { background: var(--accent); color: #1d1233; border-radius: 999px; padding: 0 7px; font-size: 0.75rem; }
  .drawer {
    position: fixed; z-index: 85; right: 0; bottom: 0; width: min(520px, 100%); max-height: 78dvh; overflow: auto;
    background: var(--surface); color: var(--text); border: 1px solid var(--border);
    border-radius: 18px 18px 0 0; padding: 14px 14px calc(70px + env(safe-area-inset-bottom));
    box-shadow: 0 -10px 40px rgba(0, 0, 0, 0.35);
  }
  @media (min-width: 700px) {
    .drawer { right: 12px; bottom: 60px; border-radius: 18px; padding-bottom: 14px; max-height: 70dvh; }
  }
  .head { display: flex; justify-content: space-between; align-items: center; gap: 10px; }
  .head h3 { margin: 0; }
  ul { list-style: none; margin: 8px 0 0; padding: 0; }
  li { padding: 10px 0; border-top: 1px solid var(--border); }
  .line { display: flex; gap: 8px; align-items: center; min-width: 0; }
  .kind { font-size: 0.7rem; font-weight: 900; background: var(--surface2); border-radius: 6px; padding: 2px 6px; flex: none; }
  .k-ws { background: var(--blue); color: #fff; }
  .k-page { background: var(--brand); color: var(--brand-ink); }
  .path { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 0.85rem; }
  .status { font-weight: 900; font-size: 0.85rem; color: var(--ok); }
  .status.bad { color: var(--bad); }
  .chips2 { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; align-items: center; }
  .hops { font-size: 0.75rem; font-weight: 700; color: var(--muted); margin-top: 4px; }
  .hops.ok { color: var(--ok); }
  .small { font-size: 0.8rem; }
</style>
