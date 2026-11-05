<script lang="ts">
  let { token }: { token: string } = $props();
  const name = $derived(token.split('@')[0].trim());
  const node = $derived(token.includes('@') ? token.split('@').slice(1).join('@') : '');
</script>

{#if name === 'gateway'}
  <span class="tc muted-chip" title={token}>routing{node ? ' @' + node : ''}</span>
{:else}
  <span class="tc svc-{name}" title={token}>{name}{#if node}<small>@{node}</small>{/if}</span>
{/if}

<style>
  .tc {
    display: inline-flex; align-items: baseline; gap: 1px;
    font-size: 0.75rem; font-weight: 800; padding: 2px 8px; border-radius: 999px;
    background: var(--brand); color: var(--brand-ink); white-space: nowrap;
  }
  .tc small { font-weight: 600; opacity: 0.85; font-size: 0.7rem; }
  .muted-chip { background: transparent; color: var(--muted); border: 1px dashed var(--border); font-weight: 600; }
  .svc-frontend { background: #7a3fe0; color: #fff; }
  .svc-account { background: #1368ce; color: #fff; }
  .svc-quiz { background: #26890c; color: #fff; }
  .svc-game { background: #c2410c; color: #fff; }
  .svc-redis { background: #b91c1c; color: #fff; }
  .svc-postgres { background: #0f766e; color: #fff; }
</style>
