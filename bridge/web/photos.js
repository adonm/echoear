import {request} from "./api.js";

const activePhotos = new Map();
const photoHref = /^photo:(photo-[0-9a-f]{8})$/;

function retainPhotos(ids) {
  for (const [id, entry] of activePhotos) {
    if (ids.has(id)) continue;
    entry.request.abort();
    if (entry.url) URL.revokeObjectURL(entry.url);
    activePhotos.delete(id);
  }
}

function clearPhotos() {
  retainPhotos(new Set());
}

function stagePhotos(svg, token) {
  const ids = new Set();
  for (const image of svg.querySelectorAll("image")) {
    const match = photoHref.exec(image.getAttribute("href") || "");
    image.removeAttribute("href");
    if (!match) continue;
    const [, id] = match;
    ids.add(id);
    let entry = activePhotos.get(id);
    if (!entry) {
      entry = {request:new AbortController()};
      activePhotos.set(id, entry);
      entry.promise = request(token, `/api/photos/${id}`, {signal:entry.request.signal})
        .then(res => res.ok ? res.blob() : null)
        .then(blob => {
          if (blob?.type === "image/png" && !entry.request.signal.aborted) {
            entry.url = URL.createObjectURL(blob);
          }
        }).catch(() => {});
    }
    if (entry.url) image.setAttribute("href", entry.url);
    else entry.promise.then(() => {
      if (entry.url && !entry.request.signal.aborted && image.isConnected) image.setAttribute("href", entry.url);
    });
  }
  retainPhotos(ids);
}

export {clearPhotos, stagePhotos};
