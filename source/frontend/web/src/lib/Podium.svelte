<script lang="ts">
  import type { ScoreRow } from './types';
  let { rows }: { rows: ScoreRow[] } = $props();
  // visual order: 2nd, 1st, 3rd
  const order = $derived([1, 0, 2].filter((i) => rows[i]).map((i) => ({ r: rows[i], place: i + 1 })));
</script>

<div class="podium">
  {#each order as o (o.r.id)}
    <div class="col p{o.place}">
      <div class="name">{o.r.nick}</div>
      <div class="muted score" style="margin-bottom:6px">{o.r.score.toLocaleString()}</div>
      <div class="block">{o.place}</div>
    </div>
  {/each}
</div>
