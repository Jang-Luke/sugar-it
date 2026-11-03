<script lang="ts">
  import { createEventDispatcher, onMount } from 'svelte'
  import { clampMenu } from '../lib/menu'

  export let x = 0
  export let y = 0
  export let label = ''
  export let items: { id: string; label: string; enabled: boolean; danger?: boolean }[] = []

  const dispatch = createEventDispatcher<{ select: string; close: void }>()

  let w = 0
  let h = 0
  let vw = window.innerWidth
  let vh = window.innerHeight
  $: pos = clampMenu({ x, y }, { w, h }, { w: vw, h: vh })

  function close() {
    dispatch('close')
  }

  function outside(ev: MouseEvent) {
    if (!(ev.target as HTMLElement | null)?.closest('.ctx')) close()
  }

  onMount(() => {
    const key = (ev: KeyboardEvent) => ev.key === 'Escape' && close()
    // scroll does not bubble, so listen while it is captured on the way down.
    window.addEventListener('mousedown', outside, true)
    window.addEventListener('keydown', key, true)
    window.addEventListener('scroll', close, true)
    window.addEventListener('wheel', close, true)
    window.addEventListener('blur', close)
    window.addEventListener('resize', close)
    return () => {
      window.removeEventListener('mousedown', outside, true)
      window.removeEventListener('keydown', key, true)
      window.removeEventListener('scroll', close, true)
      window.removeEventListener('wheel', close, true)
      window.removeEventListener('blur', close)
      window.removeEventListener('resize', close)
    }
  })
</script>

<!-- svelte-ignore a11y-no-static-element-interactions -->
<div
  class="ctx"
  role="menu"
  tabindex="-1"
  aria-label={label}
  style="left: {pos.x}px; top: {pos.y}px"
  bind:offsetWidth={w}
  bind:offsetHeight={h}
  on:contextmenu|preventDefault
>
  {#each items as it}
    <button
      class="item"
      class:danger={it.danger}
      role="menuitem"
      disabled={!it.enabled}
      on:click={() => dispatch('select', it.id)}>{it.label}</button>
  {/each}
</div>

<style>
  .ctx {
    position: fixed; z-index: 50; min-width: 180px; padding: 4px;
    background: var(--panel); border: 1px solid var(--line); border-radius: 8px;
    box-shadow: 0 6px 20px rgba(0, 0, 0, 0.3); display: flex; flex-direction: column;
  }
  .item { text-align: left; border: 0; background: transparent; padding: 6px 12px; border-radius: 4px; }
  .item:hover:not(:disabled) { background: var(--row-hover); }
  .item:disabled { opacity: 0.45; }
  .item.danger:not(:disabled) { color: var(--danger); }
</style>
