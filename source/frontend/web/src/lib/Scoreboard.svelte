<script lang="ts">
  import type { ScoreRow } from './types';
  let { rows, limit = 5, meId = '' }: { rows: ScoreRow[]; limit?: number; meId?: string } = $props();
</script>

<ol class="list">
  {#each rows.slice(0, limit) as r, i (r.id)}
    <li class:me={r.id === meId}>
      <span class="rank">{i + 1}</span>
      <span class="grow"><b>{r.nick}</b></span>
      <span class="score">{r.score.toLocaleString()}</span>
    </li>
  {/each}
</ol>

<style>
  .me { background: color-mix(in srgb, var(--accent) 22%, transparent); border-radius: 10px; }
</style>
