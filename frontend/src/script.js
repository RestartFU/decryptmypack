function decrypt() {
    let input = document.getElementById('input').value;
    let apiUrl = "http://moyai.pro:6969/download?target=" + input;

    // Create a new XMLHttpRequest object
    let xhr = new XMLHttpRequest();

    // Open a GET request to the API endpoint
    xhr.open('GET', apiUrl, true);

    // Set the responseType to 'blob' to handle binary data
    xhr.responseType = 'blob';

    // Define a callback function to handle the response
    xhr.onload = function() {
        hideDownload();
        if (xhr.status === 200) {
            // Get the Content-Disposition header
            let contentDisposition = xhr.getResponseHeader('Content-Disposition');
            if (contentDisposition) {
                // Extract the filename from the header
                let filename = contentDisposition.split('filename=')[1];
                filename = filename.slice(1, -1); // Remove surrounding quotes
                // Create a blob URL from the response
                let blob = new Blob([xhr.response], { type: 'application/octet-stream' });
                let url = window.URL.createObjectURL(blob);

                // Create a temporary link element
                let a = document.createElement('a');
                a.href = url;
                a.download = filename; // Use the extracted filename

                // Programmatically trigger a click on the link to start the download
                document.body.appendChild(a);
                a.click();

                // Clean up the URL object
                window.URL.revokeObjectURL(url);
            } else {
                console.error('Content-Disposition header not found');
            }
        } else {
            // Retrieve error message from response body
            let reader = new FileReader();
            reader.onload = function() {
                let errorMessage = reader.result;
                showError('Error downloading file: ' + errorMessage);
            };
            reader.readAsText(xhr.response);
        }
    };

    // Define a callback function to handle errors
    xhr.onerror = function() {
        // Retrieve error message from response body
        let reader = new FileReader();
        reader.onload = function() {
            let errorMessage = reader.result;
            showError('Error downloading file: ' + errorMessage);
        };
        reader.readAsText(xhr.response);
    };

    // Send the request
    xhr.send();
    showDownload();
    hideError();
}

// Function to show the error message with fade-in effect
function showError(err) {
    const errorElement = document.getElementById('error');
    errorElement.textContent = err;
    errorElement.style.opacity = '1'; // Set opacity to 1 to show the element

    setTimeout(hideError, 15000); // Hide the error message after 3 seconds
}

// Function to hide the error message with fade-out effect
function hideError() {
    const errorElement = document.getElementById('error');
    errorElement.style.opacity = '0'; // Set opacity to 0 to hide the element
}

function showDownload() {
    const loadingElement = document.getElementById('loading');
    loadingElement.style.opacity = '1';
}

function hideDownload() {
    const loadingElement = document.getElementById('loading');
    loadingElement.style.opacity = '0';
}