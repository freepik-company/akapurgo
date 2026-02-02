document.getElementById('purge-form').addEventListener('submit', async function(event) {
    event.preventDefault();
    const messageElement = document.getElementById('message');
    messageElement.textContent = '';

    // Get form data
    const purgeType = document.getElementById('purge-type').value;
    const actionType = document.getElementById('action-type').value;
    const environment = document.getElementById('environment').value;
    const postPurgeRequest = document.getElementById('post-request').checked;
    const imBypass = document.getElementById('im-bypass').checked;
    const paths = document.getElementById('paths').value.trim().split('\n').filter(Boolean);

    if (paths.length === 0) {
        messageElement.textContent = 'Please enter at least one path or tag.';
        messageElement.className = 'message error';
        return;
    }

    try {
        const response = await fetch('/api/v1/purge', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify({
                purgeType,
                actionType,
                environment,
                postPurgeRequest,
                imBypass,
                paths
            })
        });

        if (response.ok) {
            messageElement.textContent = 'Cache purged successfully.';
            messageElement.className = 'message success';
        } else {
            const errorData = await response.json();
            messageElement.textContent = `Error: ${errorData.message || 'Failed to purge cache.'}`;
            messageElement.className = 'message error';
        }
    } catch (error) {
        messageElement.textContent = 'An unexpected error occurred. Please try again.';
        messageElement.className = 'message error';
    }
});

document.addEventListener("DOMContentLoaded", () => {
    const purgeTypeSelect = document.getElementById("purge-type");
    const pathsTextarea = document.getElementById("paths");
    const postRequestCheckbox = document.getElementById("post-request");
    const postRequestContainer = postRequestCheckbox.parentElement;
    const imBypassCheckbox = document.getElementById("im-bypass");
    const imBypassContainer = imBypassCheckbox.parentElement;

    // Definir los placeholders para cada opción
    const placeholders = {
        "urls": "https://domain.com/example/path1\nhttps://domain.com/example/path2",
        "cache-tags": "tag1\ntag2\ntag3"
    };

    // Function to update visibility of checkboxes (only visible for urls)
    const updateCheckboxesVisibility = (purgeType) => {
        if (purgeType === 'urls') {
            postRequestContainer.style.display = 'block';
            imBypassContainer.style.display = 'block';
        } else {
            postRequestContainer.style.display = 'none';
            imBypassContainer.style.display = 'none';
            postRequestCheckbox.checked = false;
            imBypassCheckbox.checked = false;
        }
    };

    // Actualizar el placeholder cuando cambie el tipo de purga
    purgeTypeSelect.addEventListener("change", (event) => {
        const selectedType = event.target.value;
        pathsTextarea.placeholder = placeholders[selectedType] || "";
        updateCheckboxesVisibility(selectedType);
    });

    // Initialize visibility on page load
    updateCheckboxesVisibility(purgeTypeSelect.value);
});