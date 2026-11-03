/** The data type every drag started inside the app carries; drags without it (Explorer files, text) are not ours. */
export const APP_DRAG_TYPE = 'application/x-sugar-it-entries'

export function isAppDrag(types: readonly string[] | undefined | null): boolean {
  return !!types && Array.from(types).includes(APP_DRAG_TYPE)
}

/**
 * A drag is recorded but the mouse moves with no button down: the drag ended
 * and its dragend never reached us (the source row was removed from the page
 * while the list scrolled). During a real drag no mouse moves are delivered.
 */
export function staleDrag(recorded: boolean, buttons: number): boolean {
  return recorded && buttons === 0
}
