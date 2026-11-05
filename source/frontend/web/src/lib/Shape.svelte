<script lang="ts">
  let { i, size = 28 }: { i: number; size?: number } = $props();
  const k = $derived(((i % 4) + 4) % 4);
</script>

<svg
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="currentColor"
  stroke="currentColor"
  stroke-width="2"
  stroke-linejoin="round"
  aria-hidden="true"
>
  {#if k === 0}
    <path d="M12 4 L21 19 H3 Z" />
  {:else if k === 1}
    <path d="M12 2.5 L21.5 12 L12 21.5 L2.5 12 Z" />
  {:else if k === 2}
    <circle cx="12" cy="12" r="9" />
  {:else}
    <rect x="4" y="4" width="16" height="16" rx="1.5" />
  {/if}
</svg>
