<script lang="ts">
  import Shape from './Shape.svelte';
  let { size = 1 }: { size?: number } = $props();
</script>

<div class="logo" style="--s:{size}">
  <span class="marks">
    {#each [0, 1, 2, 3] as i}
      <span class="m a{i}"><Shape {i} size={16 * size} /></span>
    {/each}
  </span>
  <span class="word">Quizbox</span>
</div>

<style>
  .logo { display: inline-flex; align-items: center; gap: calc(10px * var(--s)); font-weight: 900; }
  .marks { display: grid; grid-template-columns: 1fr 1fr; gap: 3px; }
  .m {
    background: var(--c); color: #fff; border-radius: 6px;
    width: calc(26px * var(--s)); height: calc(26px * var(--s));
    display: grid; place-items: center;
  }
  .word { font-size: calc(1.7rem * var(--s)); letter-spacing: -0.03em; }
</style>
