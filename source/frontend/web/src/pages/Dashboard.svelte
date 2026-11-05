<script lang="ts">
  import { onMount } from 'svelte';
  import { app } from '../lib/state.svelte';
  import { get, post, del, ApiError } from '../lib/api';
  import { navigate } from '../lib/router.svelte';
  import { hostToken } from '../lib/session';
  import type { QuizSummary } from '../lib/types';

  let quizzes = $state<QuizSummary[] | null>(null);
  let error = $state('');
  let busyId = $state<number | null>(null);
  let confirmId = $state<number | null>(null);

  onMount(() => {
    const ctl = new AbortController();
    get('/api/quizzes', ctl.signal)
      .then((r) => (quizzes = r.quizzes ?? []))
      .catch(() => {});
    return () => ctl.abort();
  });

  async function host(q: QuizSummary) {
    error = '';
    busyId = q.id;
    try {
      const r = await post('/api/games', { quizId: q.id });
      hostToken.set(r.pin, r.hostToken);
      navigate(`/host/game/${r.pin}`);
    } catch (e) {
      error = (e as ApiError).recovering ? 'Service is recovering — tap again in a moment.' : (e as Error).message;
    } finally {
      busyId = null;
    }
  }

  async function remove(q: QuizSummary) {
    error = '';
    busyId = q.id;
    try {
      await del(`/api/quizzes/${q.id}`);
      quizzes = (quizzes ?? []).filter((x) => x.id !== q.id);
      confirmId = null;
    } catch (e) {
      error = (e as ApiError).recovering ? 'Service is recovering — try again.' : (e as Error).message;
    } finally {
      busyId = null;
    }
  }

  const fmt = (s: string) => {
    const d = new Date(s);
    return isNaN(+d) ? '' : d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  };
</script>

<main class="wrap stack">
  <div class="row between">
    <h1 style="margin:0">My quizzes</h1>
    <a class="btn sm ghost" href="/">Home</a>
  </div>
  <div class="row">
    <a class="btn primary grow" href="/host/quiz/new">+ New quiz</a>
    <a class="btn grow" href="/me">History</a>
  </div>
  {#if app.user}<p class="muted" style="margin:0">Signed in as {app.user.displayName}</p>{/if}
  {#if error}<p class="err" role="alert">{error}</p>{/if}

  {#if quizzes === null}
    <p class="muted">Loading…</p>
  {:else if quizzes.length === 0}
    <div class="card center">
      <h2>No quizzes yet</h2>
      <p class="muted">Create your first quiz to host a live game.</p>
    </div>
  {:else}
    {#each quizzes as q (q.id)}
      <article class="card stack fade-in">
        <div class="row between" style="align-items:flex-start">
          <div>
            <h3 style="margin:0">{q.title}</h3>
            <span class="muted">{q.questionCount} question{q.questionCount === 1 ? '' : 's'} · updated {fmt(q.updatedAt)}</span>
          </div>
        </div>
        {#if confirmId === q.id}
          <div class="row">
            <span class="grow"><b>Delete this quiz?</b></span>
            <button class="btn sm danger" disabled={busyId === q.id} onclick={() => remove(q)}>Delete</button>
            <button class="btn sm ghost" onclick={() => (confirmId = null)}>Cancel</button>
          </div>
        {:else}
          <div class="row">
            <button class="btn go grow" disabled={busyId === q.id} onclick={() => host(q)}>
              {busyId === q.id ? 'Starting…' : 'Host live'}
            </button>
            <a class="btn" href="/host/quiz/{q.id}">Edit</a>
            <button class="btn ghost" onclick={() => (confirmId = q.id)} aria-label="Delete {q.title}">Delete</button>
          </div>
        {/if}
      </article>
    {/each}
  {/if}
</main>
