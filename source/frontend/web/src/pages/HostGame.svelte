<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { GameConn, wsBase } from '../lib/ws.svelte';
  import { useNow, toMs } from '../lib/clock.svelte';
  import { hostToken } from '../lib/session';
  import Shape from '../lib/Shape.svelte';
  import ConnPill from '../lib/ConnPill.svelte';
  import Scoreboard from '../lib/Scoreboard.svelte';
  import Podium from '../lib/Podium.svelte';

  let { pin: pinProp }: { pin: string } = $props();
  const pin = untrack(() => pinProp);

  const token = hostToken.get(pin);
  const conn = new GameConn(
    () => `${wsBase()}/ws/games/${pin}?role=host&token=${encodeURIComponent(token ?? '')}`,
    { label: `/ws/games/${pin} (host)` },
  );
  const now = useNow(100);
  const joinUrl = `${location.origin}/play?pin=${pin}`;

  onMount(() => {
    if (!token) return;
    conn.start();
    return () => conn.stop();
  });

  const g = $derived(conn.game);
  const leftMs = $derived(g?.status === 'question' && g.question ? toMs(g.question.deadline) - (now.v + conn.offset) : 0);
  const leftSec = $derived(Math.max(0, Math.ceil(leftMs / 1000)));
  const pct = $derived(g?.question ? Math.max(0, Math.min(100, (leftMs / (g.question.timeLimit * 1000)) * 100)) : 0);
  const isLast = $derived(!!g && g.qIndex + 1 >= g.qCount);

  // Auto-reveal when the countdown ends or everyone has answered. Resent every
  // 2s while the state still says "question" in case a frame was lost.
  let revealQ = -1;
  let lastRevealAt = 0;
  $effect(() => {
    const t = now.v;
    if (!g || g.status !== 'question' || !g.question) return;
    if (revealQ !== g.qIndex) {
      revealQ = g.qIndex;
      lastRevealAt = 0;
    }
    const all = g.playerCount > 0 && g.answered >= g.playerCount;
    if ((leftMs <= 0 || all) && t - lastRevealAt > 2000) {
      if (conn.send({ type: 'reveal' })) lastRevealAt = t;
    }
  });

  let copied = $state(false);
  async function copyUrl() {
    try {
      await navigator.clipboard.writeText(joinUrl);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch {
      /* ignore */
    }
  }
  function endGame() {
    if (confirm('End the game now?')) conn.send({ type: 'end' });
  }

  const maxCount = $derived(Math.max(1, ...(g?.reveal?.counts ?? [0])));
  const choices = $derived(g?.question?.choices ?? (g?.reveal?.counts ?? []).map(() => ''));
</script>

<ConnPill {conn} />

<main class="wrap wide stack">
  {#if !token}
    <div class="card center stack">
      <h2>No host key for this game</h2>
      <p class="muted">Host controls are only available in the browser tab that created the game (PIN {pin}).</p>
      <a class="btn primary" href="/host">Back to my quizzes</a>
    </div>
  {:else if conn.fatal}
    <div class="card center stack">
      <h2>{conn.fatal.code === 4404 ? 'Game not found' : 'Cannot control this game'}</h2>
      <p class="muted">{conn.fatal.code === 4404 ? 'It may have expired.' : 'The host key was rejected.'}</p>
      <a class="btn primary" href="/host">Back to my quizzes</a>
    </div>
  {:else if !g}
    <p class="center muted" style="margin-top:30vh">Connecting to game {pin}…</p>
  {:else if g.status === 'lobby'}
    <section class="lobby center fade-in">
      <p class="muted" style="margin:0"><b>{g.quizTitle}</b></p>
      <p class="muted" style="margin:6px 0 0">Join at <b class="url">{joinUrl}</b></p>
      <div class="bigpin" aria-label="Game PIN {pin}">{pin}</div>
      <div class="row" style="justify-content:center">
        <button class="btn sm" onclick={copyUrl}>{copied ? 'Copied' : 'Copy join link'}</button>
      </div>
      <h2 style="margin-top:28px">{g.playerCount} player{g.playerCount === 1 ? '' : 's'} joined</h2>
      <div class="chips" style="justify-content:center;min-height:50px">
        {#each g.players as p (p.id)}<span class="chip">{p.nick}</span>{/each}
        {#if g.players.length === 0}<span class="muted">Waiting for players…</span>{/if}
      </div>
      <div class="actions">
        <button class="btn go big" disabled={g.playerCount === 0 || !conn.online} onclick={() => conn.send({ type: 'start' })}>Start game</button>
      </div>
    </section>
  {:else if g.status === 'question' && g.question}
    <section class="stack fade-in">
      <div class="row between">
        <span class="chip">Question {g.qIndex + 1} / {g.qCount}</span>
        <span class="chip">{g.answered} / {g.playerCount} answered</span>
        <div class="count" aria-label="{leftSec} seconds left">{leftSec}</div>
      </div>
      <div class="timer" class:low={leftSec <= 5}><i style="width:{pct}%"></i></div>
      <h1 class="qtext">{g.question.text}</h1>
      <div class="choices">
        {#each g.question.choices as c, i}
          <div class="tile a{i}"><Shape {i} size={40} /><span>{c}</span></div>
        {/each}
      </div>
      <div class="row" style="justify-content:flex-end">
        <button class="btn sm ghost" onclick={() => conn.send({ type: 'reveal' })}>Skip to answer</button>
      </div>
    </section>
  {:else if g.status === 'reveal'}
    <section class="stack fade-in">
      <div class="row between">
        <span class="chip">Question {g.qIndex + 1} / {g.qCount}</span>
        <span class="chip">{g.answered} / {g.playerCount} answered</span>
      </div>
      {#if g.question}<h1 class="qtext">{g.question.text}</h1>{/if}
      <div class="bars" style="--n:{g.reveal?.counts.length ?? 4}">
        {#each g.reveal?.counts ?? [] as n, i}
          {@const right = i === g.reveal?.correct}
          <div class="bar a{i}" class:dim={!right}>
            <div class="barnum">{n}</div>
            <div class="barfill" style="height:{Math.max(6, (n / maxCount) * 100)}%"></div>
            <div class="barlabel tile" class:right>
              <Shape {i} size={28} />
              <span>{choices[i]}</span>
              {#if right}<span class="check" aria-label="Correct answer">&#10003;</span>{/if}
            </div>
          </div>
        {/each}
      </div>
      <div class="card">
        <h2>Top 5</h2>
        <Scoreboard rows={g.scoreboard ?? []} limit={5} />
      </div>
      <div class="actions">
        <button class="btn primary big" disabled={!conn.online} onclick={() => conn.send({ type: 'next' })}>
          {isLast ? 'Show final results' : 'Next question'}
        </button>
      </div>
    </section>
  {:else if g.status === 'finished'}
    <section class="stack fade-in">
      <h1 class="center">Final results</h1>
      <p class="center muted" style="margin-top:-8px">{g.quizTitle}</p>
      <Podium rows={g.scoreboard ?? []} />
      <div class="card">
        <h2>All players</h2>
        <Scoreboard rows={g.scoreboard ?? []} limit={999} />
      </div>
      <a class="btn primary big" href="/host">Back to my quizzes</a>
    </section>
  {/if}

  {#if g && g.status !== 'finished' && g.status !== 'lobby'}
    <div class="center"><button class="btn sm ghost" onclick={endGame}>End game</button></div>
  {/if}
</main>

<style>
  .lobby { padding-top: 4vh; }
  .url { font-size: 1.1rem; word-break: break-all; }
  .bigpin {
    font-size: clamp(3.5rem, 18vw, 9rem); font-weight: 900; letter-spacing: 0.12em; padding-left: 0.12em;
    line-height: 1.1; margin: 8px 0 16px; background: var(--surface); border: 3px solid var(--border);
    border-radius: 24px; display: inline-block; padding-inline: 0.4em; font-variant-numeric: tabular-nums;
    box-shadow: var(--shadow); animation: pop 0.35s ease-out;
  }
  .actions { max-width: 420px; margin: 24px auto 0; }
  .qtext { text-align: center; margin: 8px 0 16px; font-size: clamp(1.5rem, 5vw, 2.6rem); overflow-wrap: anywhere; }
  .choices { display: grid; grid-template-columns: 1fr; gap: 12px; }
  .choices .tile { min-height: 84px; font-size: clamp(1.1rem, 3.4vw, 1.8rem); }
  @media (min-width: 700px) { .choices { grid-template-columns: 1fr 1fr; } }

  .bars { display: grid; grid-template-columns: repeat(var(--n), 1fr); gap: 10px; align-items: end; height: 340px; }
  .bar { position: relative; display: flex; flex-direction: column; justify-content: flex-end; height: 100%; min-width: 0; }
  .barfill { background: var(--c); border-radius: 12px 12px 0 0; transition: height 0.5s ease-out; }
  .barnum { text-align: center; font-weight: 900; font-size: 1.6rem; }
  .bar.dim .barfill { opacity: 0.45; }
  .bar.dim .barlabel { opacity: 0.55; }
  .barlabel { border-radius: 0 0 12px 12px; padding: 8px 10px; font-size: clamp(0.8rem, 2.4vw, 1.1rem); gap: 8px; flex-wrap: wrap; box-shadow: none; }
  .barlabel.right { outline: 4px solid var(--ok); outline-offset: -2px; }
  .check { margin-left: auto; font-size: 1.4rem; }
</style>
