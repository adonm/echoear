app = (() => {
  "use strict";
  const content = storeGet("content");
  const d = {...templateData, ...content};
  const title = content?.title || templateTitle;
  const theme = d.theme || "warm";
  const palettes = {
    warm: ["#6F423D", "#FFFAEE", "#392429", "#A45B48"],
    mint: ["#17463F", "#E9FFF5", "#163A33", "#27826E"],
    night: ["#141825", "#242B3F", "#F2F4FB", "#ACB9ED"],
    sky: ["#254C69", "#EDF7FF", "#192E48", "#437DA0"],
  };
  const [bg, paper, ink, accent] = palettes[theme];
  const defaults = ["#E54962", "#FFB82E", "#127D79", "#465DD1"];
  const assert = (ok, msg) => { if (!ok) throw new TypeError(msg); };
  const count = (a, lo, hi) => Array.isArray(a) && a.length >= lo && a.length <= hi;
  const integer = (n, lo, hi) => Number.isInteger(n) && n >= lo && n <= hi;
  const label = (s, n) => typeof s === "string" && s.trim() && s.length <= n;
  const color = s => /^#[0-9a-f]{6}$/i.test(s);
  const colors = () => {
    const c = d.colors || defaults;
    assert(count(c, 2, 4) && c.every(color), "colors requires 2–4 #RRGGBB colors");
    return c;
  };
  const rect = (x,y,w,h,r,c) => `<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="${r}" fill="${c}"/>`;
  const dot = (x,y,r,c) => `<circle cx="${x}" cy="${y}" r="${r}" fill="${c}"/>`;
  const txt = (x,y,value,size=18) => `<text x="${x}" y="${y}" fill="${ink}" text-anchor="middle" font-size="${size}" font-weight="bold">${ui.esc(Array.from(String(value)).slice(0, 20).join(""))}</text>`;
  const hit = (id,x,y,w,h,art) => `<g data-id="${id}" data-hit="${x},${y},${w},${h}">${art}</g>`;
  const button = (id,x,y,w,h,value,c=paper) => hit(id,x,y,w,h,rect(x,y,w,h,12,c)+txt(x+w/2,y+h/2+6,value,16));
  const screen = art => `<svg viewBox="0 0 360 360">${rect(0,0,360,360,0,bg)}${rect(49,55,262,235,22,paper)}${txt(180,78,title)}${art}</svg>`;
  const choice = (id, label) => ({id, label: String(label)});
  const menu = (badge, prompt, choices, footer="") => ui.choices({title, theme, badge, prompt, choices, footer});
  const tap = () => chime("tap");
  const win = () => chime("celebrate");
  const icon = (shape,x,y,c) => {
    const art = {
      star: '<path d="M0 -15 L4 -5 L15 -4 L7 3 L9 14 L0 8 L-9 14 L-7 3 L-15 -4 L-4 -5 Z"/>',
      heart: '<path d="M0 14 C-27 -4 -9 -19 0 -8 C9 -19 27 -4 0 14 Z"/>',
      triangle: '<path d="M0 -15 L15 13 L-15 13 Z"/>',
      diamond: '<path d="M0 -15 L15 0 L0 15 L-15 0 Z"/>',
      square: '<rect x="-12" y="-12" width="24" height="24" rx="3"/>',
      circle: '<circle r="13"/>',
    };
    return `<g transform="translate(${x} ${y})" fill="${c}">${art[shape]}</g>`;
  };

  // Quiz and picture math share the answer, feedback, and replay loop.
  function questions(qs, picture) {
    const fresh = () => ({q:0,score:0,picked:-1});
    return {
      init: fresh,
      render(s) {
        if (s.q >= qs.length) return menu("All done", `${s.score} of ${qs.length} correct`, [choice("again","Play again")]);
        const q = qs[s.q];
        const answered = s.picked >= 0;
        let svg = menu(`${s.q+1} / ${qs.length}`, answered ? `Answer: ${q.choices[q.answer]}` : q.prompt,
          answered ? [choice("next",s.q+1 === qs.length ? "Results" : "Next")] : q.choices.map((v,i) => choice("answer-"+i,v)),
          answered ? (q.answer === s.picked ? "Correct!" : "Good try!") : "");
        if (picture && !answered) {
          let art = "";
          for (const [n,y,c] of [[q.left,150,"#E54962"],[q.right,169,"#127D79"]]) {
            for (let i=0;i<n;i++) art += dot(180+(i-(n-1)/2)*21,y,7,c);
          }
          svg = svg.replace("</svg>",art+"</svg>");
        }
        return svg;
      },
      onTap(s,_,id) {
        if (id === "again") { tap(); return fresh(); }
        if (id === "next") { tap(); return {...s,q:s.q+1,picked:-1}; }
        const q = qs[s.q];
        const i = Number(id.slice(7));
        if (q && s.picked < 0 && id.startsWith("answer-") && integer(i,0,q.choices.length-1)) {
          const correct = i === q.answer;
          if (correct) win(); else tap();
          return {...s,picked:i,score:s.score+Number(correct)};
        }
      },
    };
  }

  switch (templateName) {
  case "reader":
    assert((label(d.text, 60000) || count(d.pages,1,512)) && (d.fontSize === undefined || integer(d.fontSize,14,18)), "reader needs text or pages; fontSize is 14–18");
    return ui.pagedText({...d,title,theme});
  case "quiz": {
    const qs = d.questions || [{prompt:"Which one can fly?",choices:["Fish","Bird","Dog"],answer:1}];
    assert(count(qs,1,32) && qs.every(q => label(q.prompt,200) && count(q.choices,2,4) &&
      q.choices.every(c => label(c,64)) && integer(q.answer,0,q.choices.length-1)), "quiz needs 1–32 questions with 2–4 choices and a zero-based answer");
    return questions(qs,false);
  }
  case "picture-math": {
    const ps = d.problems || [{left:2,right:3},{left:4,right:1},{left:3,right:4}];
    assert(count(ps,1,32) && ps.every(p => integer(p.left,0,8) && integer(p.right,0,8)), "picture-math needs 1–32 problems with 0–8 dots per row");
    return questions(ps.map((p,i) => {
      const n = p.left+p.right, answer = i%3, choices = [n+1,n+2];
      choices.splice(answer,0,n);
      return {...p,prompt:"How many dots in all?",answer,choices};
    }),true);
  }
  case "spot-the-difference": {
    const tiles = d.tiles || [
      {left:"star",right:"star"},{left:"square",right:"square"},{left:"circle",right:"diamond"},
      {left:"heart",right:"heart"},{left:"triangle",right:"triangle"},{left:"diamond",right:"diamond"},
    ];
    assert(count(tiles,4,6) && tiles.some(t => t.left !== t.right), "spot-the-difference needs 4–6 tile pairs and at least one difference");
    const foundAll = s => tiles.every((t,i) => t.left === t.right || s.found.includes(i));
    return {
      init: () => ({found:[], note:"Tap any pair that differs"}),
      render(s) {
        let art = txt(180,107,s.note,14);
        tiles.forEach((t,i) => {
          const x = 77+(i%2)*112, y = 117+Math.floor(i/2)*49;
          const c = s.found.includes(i) ? "#57AB75" : accent;
          art += hit("pair-"+i,x,y,94,32,rect(x,y,94,32,8,c)+icon(t.left,x+24,y+16,paper)+icon(t.right,x+70,y+16,paper));
        });
        if (foundAll(s)) art += button("again",137,265,86,22,"Again");
        return screen(art);
      },
      onTap(s,_,id) {
        if (id === "again") return this.init();
        const i = Number(id.slice(5)), t = tiles[i];
        if (!id.startsWith("pair-") || !t || s.found.includes(i)) return;
        if (t.left === t.right) { tap(); return {...s,note:"That pair is the same"}; }
        const next = {found:[...s.found,i],note:"Nice find!"};
        if (foundAll(next)) { win(); next.note = "Found them all!"; } else tap();
        return next;
      },
    };
  }
  case "drawing": {
    const c = colors();
    const draw = (s,id,x,y) => {
      if (id !== "canvas" || x < 68 || x > 292 || y < 100 || y > 230) return {...s,last:null};
      const p = [x,y], [a,b] = s.last || p;
      return {...s,last:p,lines:[...s.lines.slice(-239),[a,b,x,y,s.color]]};
    };
    return {
      init: () => ({lines:[],color:0,last:null}),
      render(s) {
        let art = rect(62,94,236,140,10,bg);
        art += hit("canvas",68,100,224,130,rect(68,100,224,130,8,paper)+s.lines.map(([a,b,x,y,i]) =>
          a === x && b === y ? dot(x,y,3,c[i]) : `<line x1="${a}" y1="${b}" x2="${x}" y2="${y}" stroke="${c[i]}" stroke-width="6" stroke-linecap="round"/>`).join(""));
        c.forEach((v,i) => {
          const x = 70+i*42;
          art += hit("ink-"+i,x,247,25,26,dot(x+13,260,16,s.color === i ? ink : bg)+dot(x+13,260,11,v));
        });
        return screen(art+button("clear",239,247,49,32,"Clear"));
      },
      onTap(s,_,id,x,y) {
        if (id === "clear") return {...s,lines:[],last:null};
        const i = Number(id.slice(4));
        if (id.startsWith("ink-") && integer(i,0,c.length-1)) { tap(); return {...s,color:i,last:null}; }
        if (id === "canvas") return draw({...s,last:null},id,x,y);
      },
      onDrag(s,_,id,x,y) { return draw(s,id,x,y); },
      onUp(s) { if (s.last) return {...s,last:null}; },
    };
  }
  case "pet": {
    const name = d.name || "Bean", c = d.pet_color || "#BD85E8";
    assert(label(name,24) && color(c), "pet needs a short name and a #RRGGBB pet_color");
    const actions = ["Feed","Play","Nap"];
    const initial = {full:2,joy:2,energy:2,note:`Hello, I'm ${name}!`};
    return {
      init: () => ({...initial,...storeGet("pet")}),
      render(s) {
        let art = txt(180,101,s.note,14)+dot(180,150,34,c)+dot(155,122,12,c)+dot(205,122,12,c);
        art += dot(169,146,4,ink)+dot(191,146,4,ink)+dot(180,159,5,ink);
        ["Full","Joy","Energy"].forEach((k,i) => {
          const x = 101+i*79;
          art += txt(x,202,k,13);
          for (let j=0;j<4;j++) art += dot(x-15+j*10,218,4,j<s[k.toLowerCase()] ? accent : bg);
          art += button(actions[i].toLowerCase(),x-29,237,58,36,actions[i],c);
        });
        return screen(art);
      },
      onTap(s,_,id) {
        const lim = n => Math.min(4,Math.max(0,n));
        let next;
        if (id === "feed") next = {...s,full:lim(s.full+1),note:"Yum!"};
        if (id === "play") next = {...s,joy:lim(s.joy+1),full:lim(s.full-1),energy:lim(s.energy-1),note:"Whee!"};
        if (id === "nap") next = {...s,energy:lim(s.energy+2),note:"So cozy."};
        if (next) { tap(); storeSet("pet",next); return next; }
      },
    };
  }
  case "timer": {
    const seconds = d.seconds === undefined ? 60 : d.seconds;
    assert(integer(seconds,1,180), "timer.seconds must be 1–180; use patch.timer for longer reminders");
    return {
      init: () => ({left:seconds,end:0}),
      render(s) {
        const m = Math.floor(s.left/60), t = String(s.left%60).padStart(2,"0");
        return screen(txt(180,170,`${m}:${t}`,54)+txt(180,205,s.end ? "Counting down" : s.left ? "Ready?" : "Time is up!")+
          button(s.end ? "reset" : "start",117,228,126,44,s.end ? "Reset" : "Start",accent));
      },
      onTap(s,_,id) {
        if (id === "start") { tap(); templateTick(true); return {left:seconds,end:Date.now()+seconds*1000}; }
        if (id === "reset") { tap(); templateTick(false); return this.init(); }
      },
      onTick(s) {
        const left = Math.max(0,Math.ceil((s.end-Date.now())/1000));
        if (!left) { win(); templateTick(false); }
        return {left,end:left ? s.end : 0};
      },
    };
  }
  case "memory-match": {
    const cards = d.cards || ["Moon","Star","Sun","Cloud"];
    assert(count(cards,2,4) && cards.every(c => label(c,10)) && new Set(cards).size === cards.length, "memory-match needs 2–4 unique short card labels");
    const shuffle = () => {
      const deck = [...cards,...cards];
      for (let i=deck.length-1;i>0;i--) {
        const j = Math.floor(Math.random()*(i+1));
        [deck[i],deck[j]] = [deck[j],deck[i]];
      }
      return {deck,open:[],found:[]};
    };
    return {
      init: shuffle,
      render(s) {
        const won = s.found.length === s.deck.length;
        let art = txt(180,101,won ? "All matched!" : "Find matching cards",14);
        s.deck.forEach((v,i) => {
          const shown = s.open.includes(i) || s.found.includes(i);
          art += button("card-"+i,72+i%3*77,112+Math.floor(i/3)*50,59,32,shown ? v : "?",shown ? paper : accent);
        });
        if (won) art += button("again",133,262,94,25,"Again",accent);
        return screen(art);
      },
      onTap(s,_,id) {
        if (id === "again") { tap(); return shuffle(); }
        const i = Number(id.slice(5));
        if (!id.startsWith("card-") || !integer(i,0,s.deck.length-1) || s.found.includes(i) || s.open.includes(i)) return;
        if (s.open.length !== 1) { tap(); return {...s,open:[i]}; }
        const j = s.open[0];
        if (s.deck[i] === s.deck[j]) { win(); return {...s,open:[],found:[...s.found,j,i]}; }
        tap(); return {...s,open:[j,i]};
      },
    };
  }
  case "fireworks": {
    const c = colors();
    return {
      init: () => ({bursts:[]}),
      render(s) {
        const art = s.bursts.map(([x,y,i]) => Array.from({length:8},(_,j) => {
          const t = Math.PI*j/4, dx = Math.round(Math.cos(t)*21), dy = Math.round(Math.sin(t)*21);
          return dot(x+dx,y+dy,4,c[i])+dot(x+dx/2,y+dy/2,2,c[i]);
        }).join("")).join("");
        return screen(txt(180,273,"Tap for fireworks",14)+hit("sky",68,95,224,145,rect(68,95,224,145,14,"#141825")+art));
      },
      onTap(s,_,id,x,y) {
        if (id !== "sky") return;
        sfx("pop");
        return {bursts:[...s.bursts.slice(-5),[Math.min(266,Math.max(94,x)),Math.min(214,Math.max(121,y)),Math.floor(Math.random()*c.length)]]};
      },
    };
  }
  }
})();
