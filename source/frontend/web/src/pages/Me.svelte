<script lang="ts">
  import { onMount } from 'svelte';
  import { get } from '../lib/api';

  let data = $state<any>(null);
  onMount(() => {
    const ctl = new AbortController();
    get('/api/me/history', ctl.signal)
      .then((r) => (data = r))
      .catch(() => {});
    return () => ctl.abort();
  });
  const fmt = (s: string) => {
    const d = new Date(s);
    return isNaN(+d) ? '' : d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  };
</script>

<main class="wrap stack">
  <div class="row between">
    <h1 style="margin:0">My history</h1>
    <a class="btn sm ghost" href="/">Home</a>
  </div>
  {#if !data}
    <p class="muted">Loading…</p>
  {:else}
    <section class="card">
      <h2>Games I played</h2>
      {#if !data.played?.length}
        <p class="muted">Nothing yet. Join a game with a PIN while logged in.</p>
      {:else}
        <ul class="list">
          {#each data.played as p}
            <li>
              <span class="rank">#{p.rank}</span>
              <span class="grow"><b>{p.quizTitle}</b><br /><span class="muted">{fmt(p.finishedAt)} · PIN {p.pin}</span></span>
              <span class="score">{p.score.toLocaleString()}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
    <section class="card">
      <h2>Games I hosted</h2>
      {#if !data.hosted?.length}
        <p class="muted">You have not hosted any games.</p>
      {:else}
        <ul class="list">
          {#each data.hosted as h}
            <li>
              <span class="grow"><b>{h.quizTitle}</b><br /><span class="muted">{fmt(h.createdAt)} · PIN {h.pin}</span></span>
              <span class="chip">{h.status}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  {/if}
</main>
