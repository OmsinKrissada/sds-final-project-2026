<script lang="ts">
  import { post, ApiError } from './api';
  import { app } from './state.svelte';

  let { ondone }: { ondone?: () => void } = $props();

  let mode = $state<'login' | 'register'>('login');
  let email = $state('');
  let password = $state('');
  let displayName = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e: Event) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      const res =
        mode === 'login'
          ? await post('/api/auth/login', { email, password })
          : await post('/api/auth/register', { email, password, displayName });
      app.user = res.user;
      password = '';
      ondone?.();
    } catch (err) {
      const e = err as ApiError;
      error = e.recovering
        ? 'The account service is recovering — try again in a moment.'
        : e.status === 401
          ? 'Wrong email or password.'
          : e.status === 409
            ? 'That email is already registered.'
            : e.message;
    } finally {
      busy = false;
    }
  }
</script>

<form onsubmit={submit} class="stack">
  <div class="row tabs" role="tablist">
    <button type="button" role="tab" aria-selected={mode === 'login'} class="tab" class:on={mode === 'login'} onclick={() => (mode = 'login')}>Log in</button>
    <button type="button" role="tab" aria-selected={mode === 'register'} class="tab" class:on={mode === 'register'} onclick={() => (mode = 'register')}>Create account</button>
  </div>
  {#if mode === 'register'}
    <label class="field">Display name
      <input class="input" bind:value={displayName} maxlength="40" required autocomplete="nickname" />
    </label>
  {/if}
  <label class="field">Email
    <input class="input" type="email" bind:value={email} required autocomplete="email" />
  </label>
  <label class="field">Password
    <input class="input" type="password" bind:value={password} required minlength="6" autocomplete={mode === 'login' ? 'current-password' : 'new-password'} />
  </label>
  {#if error}<p class="err" role="alert">{error}</p>{/if}
  <button class="btn primary big" disabled={busy}>{mode === 'login' ? 'Log in' : 'Create account'}</button>
</form>

<style>
  .tabs { gap: 6px; flex-wrap: nowrap; }
  .tab {
    flex: 1; font: inherit; font-weight: 800; min-height: 44px; border-radius: 12px; cursor: pointer;
    border: 2px solid var(--border); background: transparent; color: var(--muted);
  }
  .tab.on { background: var(--brand); border-color: var(--brand); color: var(--brand-ink); }
</style>
