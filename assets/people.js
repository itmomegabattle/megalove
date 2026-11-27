const people = document.querySelector('#people')
const form = document.querySelector('.people-search')
const input = form.elements.q
const originalURL = new URL(location.href)
const directoryURL = new URL(originalURL.searchParams.has('q') ? form.action : originalURL)
let pending
let debounce

async function refreshPeople(force = false) {
  if (!force && (document.hidden || people.contains(document.activeElement))) return
  pending?.abort()
  const request = new AbortController()
  pending = request
  const query = input.value.trim()
  const url = query ? new URL(form.action) : directoryURL
  if (query) url.searchParams.set('q', query)
  try {
    const response = await fetch(url, {
      cache: 'no-store',
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(3000)]),
    })
    if (!response.ok) return
    const next = new DOMParser().parseFromString(await response.text(), 'text/html').querySelector('#people')
    if (!request.signal.aborted && next && next.innerHTML !== people.innerHTML) {
      people.replaceChildren(...next.childNodes)
    }
  } catch {
    // Keep the current list visible until the connection recovers.
  }
}

input.addEventListener('input', () => {
  pending?.abort()
  clearTimeout(debounce)
  debounce = setTimeout(() => refreshPeople(true), 180)
})
form.addEventListener('submit', event => {
  event.preventDefault()
  clearTimeout(debounce)
  refreshPeople(true)
})
setInterval(refreshPeople, 4000)
