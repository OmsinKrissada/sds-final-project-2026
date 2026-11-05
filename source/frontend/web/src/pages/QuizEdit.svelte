<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { get, post, put, ApiError } from '../lib/api';
  import { navigate } from '../lib/router.svelte';
  import Shape from '../lib/Shape.svelte';
  import type { Question } from '../lib/types';

  let { id: idProp }: { id: string } = $props();
  const id = untrack(() => idProp);
  const isNew = id === 'new';
  const LIMITS = [5, 10, 15, 20, 30, 45, 60, 90, 120];

  const blank = (): Question => ({ text: '', choices: ['', '', '', ''], correct: 0, timeLimit: 20 });

  let title = $state('');
  let questions = $state<Question[]>([blank()]);
  let loaded = $state(isNew);
  let saving = $state(false);
  let error = $state('');

  onMount(() => {
    if (isNew) return;
    const ctl = new AbortController();
    get(`/api/quizzes/${id}`, ctl.signal)
      .then((q) => {
        title = q.title;
        questions = q.questions;
        loaded = true;
      })
      .catch((e) => {
        if (e instanceof ApiError && e.status === 404) error = 'Quiz not found.';
      });
    return () => ctl.abort();
  });

  function limitsFor(q: Question) {
    return LIMITS.includes(q.timeLimit) ? LIMITS : [...LIMITS, q.timeLimit].sort((a, b) => a - b);
  }

  function addChoice(q: Question) {
    if (q.choices.length < 4) q.choices.push('');
  }
  function removeChoice(q: Question, i: number) {
    if (q.choices.length <= 2) return;
    q.choices.splice(i, 1);
    if (q.correct === i) q.correct = 0;
    else if (q.correct > i) q.correct--;
  }
  function move(i: number, d: number) {
    const j = i + d;
    if (j < 0 || j >= questions.length) return;
    [questions[i], questions[j]] = [questions[j], questions[i]];
  }
  function dup(i: number) {
    questions.splice(i + 1, 0, JSON.parse(JSON.stringify(questions[i])));
  }

  function validate(): string {
    if (!title.trim()) return 'Give your quiz a title.';
    if (title.length > 120) return 'Title is too long (max 120).';
    if (questions.length < 1 || questions.length > 50) return 'A quiz needs 1 to 50 questions.';
    for (let i = 0; i < questions.length; i++) {
      const q = questions[i];
      const n = i + 1;
      if (!q.text.trim()) return `Question ${n} needs text.`;
      if (q.text.length > 300) return `Question ${n} text is too long (max 300).`;
      if (q.choices.length < 2) return `Question ${n} needs at least 2 choices.`;
      if (q.choices.some((c) => !c.trim() || c.length > 80)) return `Question ${n}: every choice needs 1-80 characters.`;
      if (q.timeLimit < 5 || q.timeLimit > 120) return `Question ${n}: time limit must be 5-120 seconds.`;
    }
    return '';
  }

  async function save(e: Event) {
    e.preventDefault();
    error = validate();
    if (error) return;
    saving = true;
    const body = {
      title: title.trim(),
      questions: questions.map((q) => ({
        text: q.text.trim(),
        choices: q.choices.map((c) => c.trim()),
        correct: q.correct,
        timeLimit: q.timeLimit,
      })),
    };
    try {
      if (isNew) await post('/api/quizzes', body);
      else await put(`/api/quizzes/${id}`, body);
      navigate('/host');
    } catch (err) {
      error = (err as ApiError).recovering ? 'Service is recovering — press Save again in a moment.' : (err as Error).message;
    } finally {
      saving = false;
    }
  }
</script>

<main class="wrap stack">
  <div class="row between">
    <h1 style="margin:0">{isNew ? 'New quiz' : 'Edit quiz'}</h1>
    <a class="btn sm ghost" href="/host">Cancel</a>
  </div>

  {#if !loaded}
    {#if error}<p class="err">{error}</p>{:else}<p class="muted">Loading…</p>{/if}
  {:else}
    <form class="stack" onsubmit={save}>
      <label class="field">Quiz title
        <input class="input" bind:value={title} maxlength="120" placeholder="e.g. Space trivia" />
      </label>

      {#each questions as q, qi (q)}
        <section class="card stack q">
          <div class="row between">
            <h3 style="margin:0">Question {qi + 1}</h3>
            <div class="row" style="gap:6px">
              <button type="button" class="btn sm ghost" onclick={() => move(qi, -1)} disabled={qi === 0} aria-label="Move up">&uarr;</button>
              <button type="button" class="btn sm ghost" onclick={() => move(qi, 1)} disabled={qi === questions.length - 1} aria-label="Move down">&darr;</button>
              <button type="button" class="btn sm ghost" onclick={() => dup(qi)}>Copy</button>
              <button type="button" class="btn sm ghost" onclick={() => questions.splice(qi, 1)} disabled={questions.length === 1} aria-label="Remove question">Remove</button>
            </div>
          </div>
          <textarea class="input" bind:value={q.text} maxlength="300" placeholder="Type your question" rows="2"></textarea>

          {#each q.choices as _, ci}
            <div class="choice a{ci}">
              <span class="badge"><Shape i={ci} size={22} /></span>
              <input class="input" bind:value={q.choices[ci]} maxlength="80" placeholder="Answer {ci + 1}" aria-label="Answer {ci + 1}" />
              <label class="correct" title="Mark as correct">
                <input type="radio" name="correct-{qi}" checked={q.correct === ci} onchange={() => (q.correct = ci)} />
                <span>Correct</span>
              </label>
              {#if q.choices.length > 2}
                <button type="button" class="x" onclick={() => removeChoice(q, ci)} aria-label="Remove answer {ci + 1}">&times;</button>
              {/if}
            </div>
          {/each}

          <div class="row between">
            {#if q.choices.length < 4}
              <button type="button" class="btn sm" onclick={() => addChoice(q)}>+ Add answer</button>
            {:else}<span></span>{/if}
            <label class="row" style="gap:8px">
              <span class="muted"><b>Time</b></span>
              <select class="input" style="width:auto;min-height:44px" bind:value={q.timeLimit}>
                {#each limitsFor(q) as t}<option value={t}>{t}s</option>{/each}
              </select>
            </label>
          </div>
        </section>
      {/each}

      <button type="button" class="btn ghost big" onclick={() => questions.push(blank())} disabled={questions.length >= 50}>+ Add question</button>

      {#if error}<p class="err" role="alert">{error}</p>{/if}
      <div class="savebar">
        <button class="btn go big" disabled={saving}>{saving ? 'Saving…' : 'Save quiz'}</button>
      </div>
    </form>
  {/if}
</main>

<style>
  .choice { display: flex; align-items: center; gap: 8px; }
  .badge {
    flex: none; width: 44px; height: 44px; border-radius: 12px; background: var(--c); color: #fff;
    display: grid; place-items: center;
  }
  .correct {
    flex: none; display: inline-flex; align-items: center; gap: 6px; font-weight: 800; font-size: 0.85rem; color: var(--muted);
    min-height: 44px; padding: 0 8px; border-radius: 10px; cursor: pointer;
  }
  .correct input { width: 22px; height: 22px; accent-color: var(--ok); }
  .correct:has(input:checked) { color: var(--ok); background: color-mix(in srgb, var(--ok) 14%, transparent); }
  .x {
    flex: none; width: 36px; height: 36px; border-radius: 50%; border: 0; cursor: pointer; font-size: 1.3rem;
    background: var(--surface2); color: var(--muted);
  }
  .savebar {
    position: sticky; bottom: 0; padding: 10px 0 calc(10px + env(safe-area-inset-bottom));
    background: linear-gradient(transparent, var(--bg) 40%);
  }
  @media (max-width: 480px) {
    .correct span { display: none; }
  }
</style>
