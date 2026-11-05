<script lang="ts">
  import { onMount } from 'svelte';
  import { app, boot } from './lib/state.svelte';
  import { route, initRouter, navigate } from './lib/router.svelte';
  import { get } from './lib/api';
  import TraceDrawer from './lib/TraceDrawer.svelte';
  import Home from './pages/Home.svelte';
  import Login from './pages/Login.svelte';
  import Dashboard from './pages/Dashboard.svelte';
  import QuizEdit from './pages/QuizEdit.svelte';
  import HostGame from './pages/HostGame.svelte';
  import Play from './pages/Play.svelte';
  import Me from './pages/Me.svelte';

  initRouter();

  onMount(() => {
    // Account was down while rendering the page: poll until it is back, then
    // reload so the server can bootstrap the session properly.
    if (boot.recovering) {
      app.banner = `${boot.recovering} is recovering — retrying…`;
      app.bannerKind = 'recovering';
      get('/api/auth/me')
        .catch(() => {})
        .then(() => location.reload());
    } else if (!boot.session) {
      // vite dev (no SSR boot): discover the session client-side.
      get('/api/auth/me')
        .then((r) => (app.user = r.user))
        .catch(() => {});
    }
  });

  const m = $derived.by(() => {
    const p = route.path;
    let r: RegExpMatchArray | null;
    if (p === '/') return { v: 'home' as const };
    if (p === '/login') return { v: 'login' as const };
    if (p === '/host') return { v: 'dash' as const };
    if (p === '/host/quiz/new') return { v: 'edit' as const, id: 'new' };
    if ((r = p.match(/^\/host\/quiz\/(\d+)$/))) return { v: 'edit' as const, id: r[1] };
    if ((r = p.match(/^\/host\/game\/(\d+)$/))) return { v: 'hostgame' as const, pin: r[1] };
    if (p === '/play') return { v: 'play' as const };
    if (p === '/me') return { v: 'me' as const };
    return { v: 'nf' as const };
  });
</script>

{#if app.banner}
  <div class="banner" role="status" aria-live="polite">
    <span class="spinner"></span><span>{app.banner}</span>
  </div>
{/if}

{#key route.path + (m.v === 'play' ? route.search : '')}
  {#if m.v === 'home'}
    <Home />
  {:else if m.v === 'login'}
    <Login />
  {:else if m.v === 'dash'}
    <Dashboard />
  {:else if m.v === 'edit'}
    <QuizEdit id={m.id} />
  {:else if m.v === 'hostgame'}
    <HostGame pin={m.pin} />
  {:else if m.v === 'play'}
    <Play />
  {:else if m.v === 'me'}
    <Me />
  {:else}
    <main class="wrap center">
      <h1>Page not found</h1>
      <p class="muted">That page does not exist.</p>
      <a class="btn primary" href="/">Back home</a>
    </main>
  {/if}
{/key}

<TraceDrawer />
