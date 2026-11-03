<script lang="ts">
  import { windowRange } from '../lib/virtual'

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  export let items: any[] = []
  export let rowHeight = 28
  export let overscan = 8

  let el: HTMLDivElement
  let scrollTop = 0
  let viewport = 400

  $: w = windowRange({ scrollTop, viewport, rowHeight, total: items.length, overscan })
  $: visible = items.slice(w.start, w.end)

  export function scrollToTop() {
    if (el) el.scrollTop = 0
    scrollTop = 0
  }
</script>

<div class="vl" bind:this={el} bind:clientHeight={viewport} on:scroll={() => (scrollTop = el.scrollTop)}>
  <div class="space" style="height: {w.totalHeight}px">
    <div class="win" style="transform: translateY({w.offsetTop}px)">
      {#each visible as item, i (w.start + i)}
        <div class="vrow" style="height: {rowHeight}px">
          <slot {item} index={w.start + i} />
        </div>
      {/each}
    </div>
  </div>
</div>

<style>
  .vl { overflow-y: auto; position: relative; flex: 1; min-height: 0; }
  .space { position: relative; }
  .win { position: absolute; left: 0; right: 0; top: 0; will-change: transform; }
  .vrow { overflow: hidden; }
</style>
