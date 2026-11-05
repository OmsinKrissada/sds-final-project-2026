<script lang="ts">
  import type { GameConn } from './ws.svelte';
  let { conn }: { conn: GameConn } = $props();
</script>

{#if !conn.online && !conn.fatal}
  <div class="pill" role="status"><span class="spinner"></span>{conn.everOpen ? 'Reconnecting…' : 'Connecting…'}</div>
{/if}
