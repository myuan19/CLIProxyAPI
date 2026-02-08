(function() {
  'use strict';

  var iframeContainer = null;
  var iframeLoaded = false;

  function findTabBar() {
    var h1s = document.querySelectorAll('h1');
    for (var i = 0; i < h1s.length; i++) {
      var next = h1s[i].nextElementSibling;
      if (!next || next.tagName !== 'DIV') continue;
      var btns = next.querySelectorAll(':scope > button');
      if (btns.length >= 2 && btns.length <= 4) {
        var cs = window.getComputedStyle(next);
        if (cs.display === 'flex' && cs.borderBottomStyle !== 'none') {
          return { tabBar: next, buttons: Array.from(btns), h1: h1s[i] };
        }
      }
    }
    return null;
  }

  function getBaseClass(buttons) {
    if (buttons.length === 0) return '';
    var sets = buttons.map(function(b) { return new Set(b.className.split(/\s+/).filter(Boolean)); });
    var common = [];
    sets[0].forEach(function(c) {
      if (sets.every(function(s) { return s.has(c); })) common.push(c);
    });
    return common.join(' ');
  }

  function getActiveClass(buttons) {
    if (buttons.length < 2) return '';
    var sets = buttons.map(function(b) { return new Set(b.className.split(/\s+/).filter(Boolean)); });
    for (var i = 0; i < buttons.length; i++) {
      var extras = [];
      sets[i].forEach(function(c) {
        var unique = true;
        for (var j = 0; j < sets.length; j++) {
          if (j !== i && sets[j].has(c)) { unique = false; break; }
        }
        if (unique) extras.push(c);
      });
      if (extras.length > 0) return extras[0];
    }
    return '';
  }

  function findContentSibling(tabBar) {
    var el = tabBar.nextElementSibling;
    while (el) {
      if (el.id === 'dr-iframe-container') { el = el.nextElementSibling; continue; }
      return el;
    }
    return null;
  }

  function ensureIframeContainer(tabBar) {
    if (iframeContainer && iframeContainer.parentNode) return iframeContainer;
    var c = document.createElement('div');
    c.id = 'dr-iframe-container';
    c.style.cssText = 'flex:1;min-height:0;width:100%;display:none;';
    var iframe = document.createElement('iframe');
    iframe.id = 'dr-iframe';
    iframe.style.cssText = 'width:100%;border:none;min-height:640px;height:calc(100vh - 200px);';
    if (!iframeLoaded) {
      iframe.src = '/detailed-requests.html?embed=1';
      iframeLoaded = true;
    }
    c.appendChild(iframe);
    tabBar.parentNode.insertBefore(c, tabBar.nextElementSibling);
    iframeContainer = c;
    return c;
  }

  function activateOurTab(tabBar, reactButtons, ourTab) {
    var activeClass = getActiveClass(reactButtons);
    var baseClass = getBaseClass(reactButtons);

    reactButtons.forEach(function(btn) {
      if (activeClass) btn.classList.remove(activeClass);
      btn.style.color = 'var(--text-secondary)';
      btn.style.borderBottomColor = 'transparent';
    });

    ourTab.className = baseClass + (activeClass ? ' ' + activeClass : '');
    ourTab.style.color = '';
    ourTab.style.borderBottomColor = '';

    var content = findContentSibling(tabBar);
    if (content) content.style.display = 'none';

    var ic = ensureIframeContainer(tabBar);
    ic.style.display = 'flex';

    window.__dr_tab_active = true;
  }

  function deactivateOurTab(tabBar, reactButtons, ourTab) {
    reactButtons.forEach(function(btn) {
      btn.style.color = '';
      btn.style.borderBottomColor = '';
    });

    var allBtns = reactButtons.concat([ourTab]);
    var baseClass = getBaseClass(allBtns);
    ourTab.className = baseClass;
    ourTab.style.color = '';
    ourTab.style.borderBottomColor = '';

    var content = findContentSibling(tabBar);
    if (content) content.style.display = '';

    if (iframeContainer) iframeContainer.style.display = 'none';

    window.__dr_tab_active = false;
  }

  function inject() {
    var result = findTabBar();
    if (!result) {
      if (window.__dr_tab_active) {
        window.__dr_tab_active = false;
        if (iframeContainer) iframeContainer.style.display = 'none';
      }
      return;
    }

    var tabBar = result.tabBar;
    var buttons = result.buttons;

    var existingTab = tabBar.querySelector('#dr-tab');
    if (existingTab) {
      if (window.__dr_tab_active) {
        var reactBtns = Array.from(tabBar.querySelectorAll(':scope > button:not(#dr-tab)'));
        activateOurTab(tabBar, reactBtns, existingTab);
      }
      return;
    }

    var tab = document.createElement('button');
    tab.id = 'dr-tab';
    tab.type = 'button';
    var baseClass = getBaseClass(buttons);
    tab.className = baseClass;
    tab.textContent = '\u8BF7\u6C42\u8BE6\u60C5';

    tab.addEventListener('click', function(e) {
      e.preventDefault();
      e.stopPropagation();
      var reactBtns = Array.from(tabBar.querySelectorAll(':scope > button:not(#dr-tab)'));
      activateOurTab(tabBar, reactBtns, tab);
    });

    buttons.forEach(function(btn) {
      btn.addEventListener('click', function() {
        if (window.__dr_tab_active) {
          var reactBtns = Array.from(tabBar.querySelectorAll(':scope > button:not(#dr-tab)'));
          deactivateOurTab(tabBar, reactBtns, tab);
        }
      }, true);
    });

    tabBar.appendChild(tab);

    if (window.__dr_tab_active) {
      var reactBtns = Array.from(tabBar.querySelectorAll(':scope > button:not(#dr-tab)'));
      activateOurTab(tabBar, reactBtns, tab);
    }
  }

  function injectStyles() {
    if (document.getElementById('dr-inject-css')) return;
    var style = document.createElement('style');
    style.id = 'dr-inject-css';
    style.textContent = '#dr-tab{cursor:pointer;transition:color .15s ease,border-color .15s ease}#dr-tab:hover{color:var(--text-primary)}#dr-tab:focus,#dr-tab:focus-visible{outline:none;box-shadow:none}#dr-iframe-container{flex-direction:column}#dr-iframe-container iframe{flex:1}';
    document.head.appendChild(style);
  }

  function startObserving() {
    injectStyles();
    inject();
    var observer = new MutationObserver(function() { inject(); });
    observer.observe(document.body, { childList: true, subtree: true });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', startObserving);
  } else {
    startObserving();
  }
})();
