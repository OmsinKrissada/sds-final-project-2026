<script lang="ts">
  import { onMount } from 'svelte';
  import { boot, bootFresh, app } from '../lib/state.svelte';
  import { query, navigate } from '../lib/router.svelte';
  import { GameConn, wsBase } from '../lib/ws.svelte';
  import { useNow, toMs } from '../lib/clock.svelte';
  import { playerSession } from '../lib/session';
  import Shape from '../lib/Shape.svelte';
  import ConnPill from '../lib/ConnPill.svelte';
  import Scoreboard from '../lib/Scoreboard.svelte';
  import Podium from '../lib/Podium.svelte';

  const q = query();
  const pin = q.get('pin') ?? '';
  const nickParam = q.get('nick') ?? '';
  const bootGame = bootFresh.game && boot.game && boot.game.pin === pin ? boot.game : null;
  bootFresh.game = false;

  let pinInput = $state(pin);
  let nick = $state(nickParam || app.user?.displayName || '');
  let formError = $state('');
  let conn = $state<GameConn | null>(null);
  let activePin = $state(pin);
  let activeNick = '';
  const now = useNow(100);

  // Locally remembered choice per question (instant feedback before the server echoes).
  let picked = $state<Record<number, number>>({});

  function join(p: string, n: string) {
    activePin = p;
    activeNick = n;
    formError = '';
    conn?.stop();
    const c = new GameConn(
      () => {
        const s = playerSession.get(activePin);
        const base = `${wsBase()}/ws/games/${activePin}?role=player`;
        return s?.token ? `${base}&resume=${encodeURIComponent(s.token)}` : `${base}&nick=${encodeURIComponent(activeNick)}`;
      },
      {
        label: `/ws/games/${p} (player)`,
        onHello: (m) => {
          if (m.playerToken) playerSession.set(activePin, { token: m.playerToken, nick: activeNick });
        },
      },
    );
    conn = c;
    c.start();
  }

  onMount(() => {
    const stored = pin ? playerSession.get(pin) : null;
    if (pin && stored) join(pin, stored.nick);
    else if (pin && nickParam.trim()) {
      join(pin, nickParam.trim());
      history.replaceState(null, '', `/play?pin=${pin}`);
    }
    return () => conn?.stop();
  });

  // A fatal close ends the connection: clear stale credentials and explain.
  $effect(() => {
    const f = conn?.fatal;
    if (!f) return;
    playerSession.clear(activePin);
    formError =
      f.code === 4404
        ? 'Game not found. Check the PIN.'
        : f.code === 4401
          ? 'Your seat expired. Join again.'
          : f.reason || 'Could not join (is that nickname already taken?).';
    conn?.stop();
    conn = null;
  });

  function submit(e: Event) {
    e.preventDefault();
    const p = pinInput.trim();
    const n = nick.trim();
    if (!p || !n) return;
    if (p !== pin) {
      navigate(`/play?pin=${encodeURIComponent(p)}&nick=${encodeURIComponent(n)}`, true);
      return;
    }
    join(p, n);
  }

  function answer(i: number) {
    const g = conn?.game;
    if (!g || g.status !== 'question' || picked[g.qIndex] !== undefined) return;
    picked[g.qIndex] = i;
    if (!conn!.send({ type: 'answer', q: g.qIndex, choice: i })) delete picked[g.qIndex];
    try {
      navigator.vibrate?.(25);
    } catch {
      /* ignore */
    }
  }

  // Server rejected the answer: let the player try again.
  $effect(() => {
    const err = conn?.lastError;
    const g = conn?.game;
    if (err && g && g.status === 'question' && !g.you?.answered) delete picked[g.qIndex];
  });

  const g = $derived(conn?.game ?? null);
  const you = $derived(g?.you);
  const leftMs = $derived(g?.status === 'question' && g.question ? toMs(g.question.deadline) - (now.v + (conn?.offset ?? 0)) : 0);
  const leftSec = $derived(Math.max(0, Math.ceil(leftMs / 1000)));
  const pct = $derived(g?.question ? Math.max(0, Math.min(100, (leftMs / (g.question.timeLimit * 1000)) * 100)) : 0);
  const myPick = $derived(g ? picked[g.qIndex] : undefined);
  const locked = $derived(!!g && (myPick !== undefined || !!you?.answered));
  const rank = $derived.by(() => {
    if (!g?.scoreboard || !you) return 0;
    return g.scoreboard.findIndex((r) => r.id === you.id) + 1;
  });
  const showReveal = $derived(g?.status === 'reveal');
  const correct = $derived(showReveal && !!you?.answered && you.lastCorrect);
</script>

{#if conn}<ConnPill {conn} />{/if}

{#if !conn}
  <main class="wrap stack">
    <h1 class="center" style="margin-top:6vh">Join a game</h1>
    {#if bootGame}<p class="center muted" style="margin-top:-8px"><b>{bootGame.quizTitle}</b> · {bootGame.playerCount} playing</p>{/if}
    <form class="card stack" onsubmit={submit}>
      {#if !pin}
        <input
          class="input pin"
          inputmode="numeric"
          maxlength="6"
          placeholder="Game PIN"
          aria-label="Game PIN"
          value={pinInput}
          oninput={(e) => (pinInput = e.currentTarget.value.replace(/\D/g, '').slice(0, 6))}
        />
      {:else}
        <div class="center muted">PIN <b style="font-size:1.4rem;letter-spacing:0.12em">{pin}</b></div>
      {/if}
      <input class="input" maxlength="20" placeholder="Nickname" aria-label="Nickname" bind:value={nick} />
      {#if formError}<p class="err" role="alert">{formError}</p>{/if}
      <button class="btn primary big" disabled={!pinInput.trim() || !nick.trim()}>Let's go</button>
    </form>
    <p class="center"><a href="/">Back home</a></p>
  </main>
{:else if !g}
  <main class="wrap center"><p class="muted" style="margin-top:30vh">Joining game {activePin}…</p></main>
{:else if g.status === 'lobby'}
  <main class="wrap center fade-in">
    <div style="margin-top:16vh">
      <div class="done"><Shape i={0} size={34} /><Shape i={1} size={34} /><Shape i={2} size={34} /><Shape i={3} size={34} /></div>
      <h1>You're in{you ? `, ${you.nick}` : ''}!</h1>
      <p class="muted">{g.quizTitle}</p>
      <p class="muted">Waiting for the host to start… {g.playerCount} player{g.playerCount === 1 ? '' : 's'} here.</p>
    </div>
  </main>
{:else if g.status === 'question' && g.question}
  <div class="play">
    <header>
      <span class="chip">{g.qIndex + 1} / {g.qCount}</span>
      <div class="timer grow" class:low={leftSec <= 5}><i style="width:{pct}%"></i></div>
      <span class="count" style="font-size:1.3rem">{leftSec}</span>
    </header>
    {#if locked}
      <div class="locked fade-in">
        {#if myPick !== undefined}
          <div class="tile a{myPick} lockedtile"><Shape i={myPick} size={64} /></div>
        {/if}
        <h1>Answer locked in</h1>
        <p class="muted">Waiting for everyone else…</p>
      </div>
    {:else}
      <p class="qt">{g.question.text}</p>
      <div class="grid n{g.question.choices.length}">
        {#each g.question.choices as c, i}
          <button class="tile a{i}" disabled={leftMs <= 0 || !conn?.online} onclick={() => answer(i)}>
            <Shape {i} size={44} /><span>{c}</span>
          </button>
        {/each}
      </div>
      {#if leftMs <= 0}<p class="center muted">Time's up!</p>{/if}
    {/if}
  </div>
{:else if g.status === 'reveal'}
  <main class="wrap center fade-in">
    <div class="verdict" class:ok={correct} class:bad={!correct}>
      <h1>{correct ? 'Correct!' : you?.answered ? 'Incorrect' : "Time's up"}</h1>
      <div class="pts">+{correct ? (you?.lastPoints ?? 0).toLocaleString() : 0}</div>
    </div>
    {#if g.reveal && g.question}
      <p class="muted">Correct answer</p>
      <div class="tile a{g.reveal.correct}" style="justify-content:center">
        <Shape i={g.reveal.correct} size={30} /><span>{g.question.choices[g.reveal.correct]}</span>
      </div>
    {/if}
    <div class="card" style="margin-top:20px">
      <div class="row between">
        <span>Your score <b class="score">{(you?.score ?? 0).toLocaleString()}</b></span>
        <span class="chip">{rank ? `Rank #${rank}` : 'Outside top 10'}</span>
      </div>
    </div>
    <p class="muted">Next question coming up…</p>
  </main>
{:else if g.status === 'finished'}
  <main class="wrap center stack fade-in">
    <h1 style="margin-top:4vh">{rank === 1 ? 'You won!' : 'Game over'}</h1>
    <div class="card">
      <div class="muted">Final rank</div>
      <div class="bigrank">{rank ? `#${rank}` : '-'}</div>
      <div class="score" style="font-size:1.4rem">{(you?.score ?? 0).toLocaleString()} points</div>
    </div>
    <Podium rows={(g.scoreboard ?? []).slice(0, 3)} />
    <div class="card" style="text-align:left"><Scoreboard rows={g.scoreboard ?? []} limit={10} meId={you?.id} /></div>
    <a class="btn primary big" href="/">Play again</a>
  </main>
{/if}

<style>
  .done { display: flex; justify-content: center; gap: 10px; margin-bottom: 10px; }
  .done :global(svg:nth-child(1)) { color: var(--red); }
  .done :global(svg:nth-child(2)) { color: var(--blue); }
  .done :global(svg:nth-child(3)) { color: var(--yellow); }
  .done :global(svg:nth-child(4)) { color: var(--green); }
  .play { display: flex; flex-direction: column; min-height: 100dvh; padding: 12px; gap: 10px; }
  .play header { display: flex; align-items: center; gap: 10px; }
  .qt { margin: 0; font-weight: 800; text-align: center; font-size: 1.05rem; display: -webkit-box; -webkit-line-clamp: 3; line-clamp: 3; -webkit-box-orient: vertical; overflow: hidden; }
  .grid { flex: 1; display: grid; gap: 12px; grid-template-columns: 1fr 1fr; grid-auto-rows: 1fr; padding-bottom: calc(52px + env(safe-area-inset-bottom)); }
  .grid.n2 { grid-template-columns: 1fr; }
  .grid.n3 > :last-child { grid-column: 1 / -1; }
  .grid .tile { flex-direction: column; justify-content: center; text-align: center; font-size: clamp(1.1rem, 5vw, 1.7rem); padding: 12px; }
  .grid.n2 .tile { flex-direction: row; justify-content: flex-start; text-align: left; }
  .grid .tile:disabled { opacity: 0.5; }
  .locked { flex: 1; display: flex; flex-direction: column; align-items: center; justify-content: center; text-align: center; }
  .lockedtile { width: 120px; height: 120px; justify-content: center; border-radius: 28px; margin-bottom: 20px; animation: pop 0.3s ease-out; }
  .verdict { border-radius: 24px; padding: 28px 16px; margin-top: 6vh; margin-bottom: 18px; color: #fff; }
  .verdict.ok { background: var(--ok); color: #06260f; }
  .verdict.bad { background: var(--bad); color: #fff; }
  .verdict h1 { margin: 0; }
  .pts { font-size: 2.6rem; font-weight: 900; }
  .bigrank { font-size: 4.5rem; font-weight: 900; line-height: 1.1; color: var(--brand); }
</style>
