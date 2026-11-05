<script lang="ts">
  import { app } from '../lib/state.svelte';
  import { navigate, query } from '../lib/router.svelte';
  import Logo from '../lib/Logo.svelte';
  import Auth from '../lib/Auth.svelte';

  function done() {
    const next = query().get('next');
    navigate(next && next.startsWith('/') && !next.startsWith('//') ? next : '/host', true);
  }
  $effect(() => {
    if (app.user) done();
  });
</script>

<main class="wrap stack">
  <div class="hero-title"><a href="/" style="text-decoration:none;color:inherit"><Logo /></a></div>
  <section class="card">
    <h2>Log in to continue</h2>
    <Auth ondone={done} />
  </section>
</main>
