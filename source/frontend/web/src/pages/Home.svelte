<script lang="ts">
  import { onMount } from 'svelte';
  import { app, boot, bootFresh } from '../lib/state.svelte';
  import { navigate } from '../lib/router.svelte';
  import { get, post } from '../lib/api';
  import Logo from '../lib/Logo.svelte';
  import Auth from '../lib/Auth.svelte';

  let pin = $state('');
  let nick = $state(app.user?.displayName ?? '');
  let nickTouched = false;
  let hof = $state<{ userId: number; displayName: string; score: number }[] | null>(null);

  $effect(() => {
    if (!nickTouched && app.user && !nick) nick = app.user.displayName;
  });

  onMount(() => {
    const ctl = new AbortController();
    if (bootFresh.hallOfFame && boot.hallOfFame) hof = boot.hallOfFame.entries ?? [];
    else
      get('/api/leaderboard?limit=5', ctl.signal)
        .then((r) => (hof = r.entries ?? []))
        .catch(() => {});
    bootFresh.hallOfFame = false;
    return () => ctl.abort();
  });

  function join(e: Event) {
    e.preventDefault();
    navigate(`/play?pin=${encodeURIComponent(pin)}&nick=${encodeURIComponent(nick.trim())}`);
  }

  async function logout() {
    try {
      await post('/api/auth/logout');
      app.user = null;
    } catch {
      /* banner shows */
    }
  }
</script>

<main class="wrap stack">
  <div class="hero-title"><Logo size={1.3} /></div>
  <p class="center muted" style="margin-top:-6px">Live quiz games. Join with a PIN, or host your own.</p>

  <form class="card stack join" onsubmit={join}>
    <label class="sr" for="pin">Game PIN</label>
    <input
      id="pin"
      class="input pin"
      inputmode="numeric"
      pattern="[0-9]*"
      autocomplete="off"
      maxlength="6"
      placeholder="Game PIN"
      value={pin}
      oninput={(e) => (pin = e.currentTarget.value.replace(/\D/g, '').slice(0, 6))}
    />
    <label class="sr" for="nick">Nickname</label>
    <input
      id="nick"
      class="input"
      maxlength="20"
      placeholder="Nickname"
      autocomplete="nickname"
      bind:value={nick}
      oninput={() => (nickTouched = true)}
    />
    <button class="btn primary big" disabled={pin.length < 4 || !nick.trim()}>Join game</button>
  </form>

  <section class="card">
    <h2>Hall of fame</h2>
    {#if hof === null}
      <p class="muted">
        {boot.degraded?.includes('game') ? 'Scores are recovering — retrying…' : 'Loading…'}
      </p>
    {:else if hof.length === 0}
      <p class="muted">No champions yet. Be the first!</p>
    {:else}
      <ol class="list">
        {#each hof as h, i}
          <li>
            <span class="rank">{i + 1}</span>
            <span class="grow"><b>{h.displayName}</b></span>
            <span class="score">{h.score.toLocaleString()}</span>
          </li>
        {/each}
      </ol>
    {/if}
  </section>

  <section class="card">
    {#if app.user}
      <h2>Hi, {app.user.displayName}</h2>
      <div class="row">
        <a class="btn primary grow" href="/host">Host dashboard</a>
        <a class="btn grow" href="/me">My history</a>
      </div>
      <p style="margin:14px 0 0"><button class="btn sm ghost" onclick={logout}>Log out</button></p>
    {:else}
      <h2>Host a game</h2>
      <p class="muted">Log in to create quizzes and host live games.</p>
      <Auth />
    {/if}
  </section>
</main>
