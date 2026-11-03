<script lang="ts">
  import { createEventDispatcher } from 'svelte'
  import { ko } from '../ko'

  export let title: string
  export let message = ''
  export let names: string[] = []
  export let extra = '' // e.g. "외 3개"
  export let input: string | null = null // a text field, preset to this value
  export let inputLabel = ''
  export let confirmLabel: string = ko.common.ok
  export let danger = false
  export let busy = false

  const dispatch = createEventDispatcher<{ confirm: string; cancel: void }>()
  let value = input ?? ''

  function confirm() {
    if (busy) return
    if (input !== null && value.trim() === '') return
    dispatch('confirm', value)
  }
</script>

<!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
<div class="backdrop" on:click|self={() => dispatch('cancel')}>
  <div class="dlg" role="dialog" aria-modal="true" aria-label={title}>
    <h3>{title}</h3>
    {#if message}<p>{message}</p>{/if}
    {#if names.length}
      <ul class="names">
        {#each names as n}<li><strong>{n}</strong></li>{/each}
        {#if extra}<li class="muted">{extra}</li>{/if}
      </ul>
    {/if}
    {#if input !== null}
      <label class="field">
        <span class="muted">{inputLabel}</span>
        <!-- svelte-ignore a11y-autofocus -->
        <input bind:value autofocus on:keydown={(e) => e.key === 'Enter' && confirm()} />
      </label>
    {/if}
    <div class="row actions">
      <button on:click={() => dispatch('cancel')}>{ko.common.cancel}</button>
      <button class:danger class:primary={!danger} disabled={busy} on:click={confirm}>{confirmLabel}</button>
    </div>
  </div>
</div>

<style>
  .backdrop {
    position: fixed; inset: 0; background: rgba(0, 0, 0, 0.4);
    display: flex; align-items: center; justify-content: center; z-index: 10;
  }
  .dlg {
    background: var(--panel); border: 1px solid var(--line); border-radius: 10px;
    padding: 18px 20px; min-width: 340px; max-width: 520px;
  }
  h3 { margin: 0 0 8px; }
  .names { margin: 8px 0; padding-left: 20px; max-height: 180px; overflow: auto; word-break: break-all; }
  .field { display: flex; flex-direction: column; gap: 4px; margin: 8px 0; }
  .actions { justify-content: flex-end; margin-top: 12px; }
</style>
